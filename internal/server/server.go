// Package server exposes the local OpenAI/Anthropic-compatible endpoints.
package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"mimo-switch/internal/modelmap"
	"mimo-switch/internal/quota"
	"mimo-switch/internal/store"
	"mimo-switch/internal/upstream"
	"mimo-switch/internal/usage"
	"mimo-switch/internal/wire"
)

type Server struct {
	client       *upstream.Client
	cfg          *store.Config
	listen       string
	localToken   string
	requireToken bool
	tracker      *usage.Tracker
	startedAt    time.Time

	// sessionMu guards the live credential and catalogue. Renewal publishes copies rather
	// than mutating them, so a handler can never read a half-written string or slice header.
	sessionMu sync.RWMutex
	cred      *store.Credential
	modelList []usage.CatalogModel

	refreshLock sync.Mutex
	refreshedAt time.Time

	quotaMu    sync.Mutex
	quotaValue *quota.Usage
	quotaAt    time.Time

	resolver *modelmap.Resolver

	// relogin overrides the renewal path; nil means use MiMo's passport flow.
	relogin relogin
}

// quotaTTL bounds how often the panel can refresh the allowance from MiMo.
const quotaTTL = 60 * time.Second

func New(cfg *store.Config) (*Server, error) {
	cred := cfg.Credential()
	if cred == nil {
		return nil, errors.New("尚未签发凭证，请先运行 mimo-switch login")
	}
	client := upstream.New(cred.SK, cred.BaseURL)
	if cred.Kind == store.KindDesktop {
		client = upstream.NewCookie(cred.Cookie, cred.BaseURL)
	}

	// MiMo renames its models on every generation (x-preview → v2.6), so the catalogue is
	// read from the app's own cache rather than hardcoded.
	models, err := usage.LoadTextModels()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读不到 MiMo 模型目录，沿用内置倍率: %v\n", err)
	}
	s := &Server{
		client:       client,
		cred:         cred,
		cfg:          cfg,
		listen:       cfg.Listen,
		localToken:   cfg.LocalToken,
		requireToken: cfg.RequireToken,
		tracker:      usage.NewTracker(models),
		startedAt:    time.Now(),
		modelList:    models,
	}
	s.pinModel()
	aliases := cfg.ModelAliases
	if len(aliases) == 0 {
		aliases = modelmap.DefaultAliases()
	}
	s.resolver = modelmap.New(s.catalogIDs(), aliases, s.credential().Model)
	return s, nil
}

// credential returns the live session. Treat the result as read-only: renewal swaps in a
// copy instead of editing the published one.
func (s *Server) credential() *store.Credential {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.cred
}

// catalogue returns the model list read from MiMo's own cache.
func (s *Server) catalogue() []usage.CatalogModel {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.modelList
}

// setSession publishes a new session, and a new catalogue when one was supplied.
func (s *Server) setSession(cred *store.Credential, models []usage.CatalogModel) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.cred = cred
	if len(models) > 0 {
		s.modelList = models
	}
}

// catalogIDs lists the real model ids the desktop currently offers.
func (s *Server) catalogIDs() []string {
	models := s.catalogue()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// pinModel drops a stored default that the current catalogue no longer offers, so clients
// asking for an unknown model are not sent a stale name. Only ever called while building
// the server, so it edits the credential directly.
func (s *Server) pinModel() {
	models := s.catalogue()
	if len(models) == 0 {
		return
	}
	for _, m := range models {
		if m.ID == s.cred.Model {
			return
		}
	}
	if preferred := usage.PreferredModel(models); preferred != "" {
		fmt.Fprintf(os.Stderr, "凭证里的模型 %q 已不在目录中，改用 %q\n", s.cred.Model, preferred)
		s.cred.Model = preferred
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /", s.handlePanel)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /v1/models", s.guard(s.handleModels))
	mux.HandleFunc("GET /models", s.guard(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.guard(s.handleChat))
	mux.HandleFunc("POST /v1/messages", s.guard(s.handleMessages))
	mux.HandleFunc("POST /v1/responses", s.guard(s.handleResponses))
	return mux
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = httpServer.Close()
	}()
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// guard rejects anything that is not a local caller presenting the token. The
// endpoint is a plain API key passthrough, so an open listener on a shared machine
// would hand the user's quota to whoever can reach the port.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			writeError(w, http.StatusForbidden, "forbidden", "only loopback clients are allowed")
			return
		}
		if !isLocalHost(r.Host) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "Host header must be localhost")
			return
		}
		if s.requireToken && !s.tokenAccepted(r) {
			writeError(w, http.StatusUnauthorized, "authentication_error",
				"missing or wrong Authorization bearer token; see mimo-switch status")
			return
		}
		next(w, r)
	}
}

func (s *Server) tokenAccepted(r *http.Request) bool {
	if s.localToken == "" {
		// Fail closed: comparing two empty strings succeeds, which would admit any caller.
		return false
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if provided == "" {
		// Claude Code and some SDKs also honour an x-api-key header.
		provided = r.Header.Get("x-api-key")
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.localToken)) == 1
}

func isLoopback(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.TrimPrefix(strings.TrimPrefix(host, "["), "]"))
	return ip != nil && ip.IsLoopback()
}

func isLocalHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"plan":     s.credential().Plan(),
		"upstream": s.credential().BaseURL,
		"key":      s.credential().Masked(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	// The desktop broker has no model listing, so advertise what the credential proved.
	if s.credential().Kind == store.KindDesktop {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.desktopModels()})
		return
	}
	res, err := s.client.Get(r.Context(), "/models")
	if err != nil {
		s.forwardError(w, err)
		return
	}
	defer res.Body.Close()
	copyStreamed(w, res)
}

func (s *Server) desktopModels() []map[string]any {
	seen := map[string]bool{}
	out := []map[string]any{}
	ids := make([]string, 0, len(s.catalogue())+1)
	ids = append(ids, s.credential().Model)
	for _, m := range s.catalogue() {
		ids = append(ids, m.ID)
	}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, map[string]any{
			"id": id, "object": "model", "created": s.credential().IssuedAt.Unix(),
			"owned_by": "xiaomi-mimo-desktop",
		})
	}
	return out
}

// rewriteModel resolves the client's model label onto a real MiMo model. Clients like
// Claude Code and Codex send their own names, which mimo-server rejects, so the name is
// mapped through the alias table; anything unmapped falls back to the default and is
// recorded so the mapping can be made explicit.
func (s *Server) rewriteModel(body []byte) []byte {
	if s.credential().Kind != store.KindDesktop || s.resolver == nil {
		return body
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Model == "" {
		return body
	}
	target, recognised := s.resolver.Resolve(probe.Model)
	s.tracker.RecordModelName(probe.Model, recognised)
	if target == probe.Model {
		return body
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		return body
	}
	generic["model"] = target
	out, err := json.Marshal(generic)
	if err != nil {
		return body
	}
	return out
}

// handleChat is a faithful passthrough: the upstream already speaks OpenAI, so the only
// job here is to relay bytes without rewriting the caller's request, while sniffing the
// usage the upstream reports.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	res, err := s.post(r.Context(), "/chat/completions", json.RawMessage(s.rewriteModel(body)))
	if err != nil {
		s.forwardError(w, err)
		return
	}
	defer res.Body.Close()
	relayAndAccount(w, res, s.credential().Model, func(model string, in, out int) {
		s.record(model, in, out, false)
	})
}

// handleMessages accepts Anthropic traffic and serves Anthropic traffic, translating
// both directions through the wire package.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}

	translated, err := wire.AnthropicToOpenAI(s.rewriteModel(body))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var probe wire.Request
	_ = json.Unmarshal(body, &probe)

	if !probe.Stream {
		res, err := s.post(r.Context(), "/chat/completions", json.RawMessage(translated))
		if err != nil {
			s.forwardError(w, err)
			return
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "api_error", "upstream read: "+err.Error())
			return
		}
		s.recordFromChatJSON(raw)
		out, err := wire.CompletionToAnthropic(raw)
		if err != nil {
			writeError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	res, err := s.post(r.Context(), "/chat/completions", json.RawMessage(translated))
	if err != nil {
		s.forwardError(w, err)
		return
	}
	defer res.Body.Close()
	s.streamAnthropic(w, r, res.Body)
}

func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, body io.Reader) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	stream := wire.NewStream()
	relayEvents(w, flusher, body, stream.Start, stream.Feed, stream.Finish)
	in, out := stream.Usage()
	s.record(s.credential().Model, in, out, false)
}

// handleResponses serves the OpenAI Responses API shape used by Codex, on top of the
// same chat upstream.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	chatBody, err := wire.ResponsesToChat(s.rewriteModel(body))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var probe wire.ResponsesRequest
	_ = json.Unmarshal(body, &probe)

	if !probe.Stream {
		res, err := s.post(r.Context(), "/chat/completions", json.RawMessage(chatBody))
		if err != nil {
			s.forwardError(w, err)
			return
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "api_error", "upstream read: "+err.Error())
			return
		}
		s.recordFromChatJSON(raw)
		out, err := wire.ChatToResponse(raw)
		if err != nil {
			writeError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	res, err := s.post(r.Context(), "/chat/completions", json.RawMessage(chatBody))
	if err != nil {
		s.forwardError(w, err)
		return
	}
	defer res.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	stream := wire.NewResponseStream()
	relayEvents(w, flusher, res.Body, stream.Start, stream.Feed, stream.Finish)
	in, out := stream.Usage()
	s.record(s.credential().Model, in, out, false)
}

// relayEvents drives one translation from an upstream OpenAI SSE body: it parses data
// frames, hands them to the protocol-specific emitter, and flushes each batch.
func relayEvents(
	w http.ResponseWriter,
	flusher http.Flusher,
	upstream io.Reader,
	start func() []wire.Event,
	feed func(wire.Chunk) []wire.Event,
	finish func() []wire.Event,
) {
	emit := func(events []wire.Event) bool {
		for _, ev := range events {
			if err := wire.WriteEvent(w, ev); err != nil {
				return false
			}
		}
		flusher.Flush()
		return true
	}
	if !emit(start()) {
		return
	}
	scanner := bufio.NewScanner(upstream)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		payload, done, ok := wire.OpenAILine(scanner.Text())
		if done {
			break
		}
		if !ok {
			continue
		}
		var chunk wire.Chunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // keep-alives and provider-specific trailer lines
		}
		if !emit(feed(chunk)) {
			return
		}
	}
	emit(finish())
}

// forwardError keeps the upstream's distinction between "credential is dead" and
// "slow down", which is what the panel needs to act on.
func (s *Server) forwardError(w http.ResponseWriter, err error) {
	var status *upstream.StatusError
	if errors.As(err, &status) {
		s.record(s.credential().Model, 0, 0, true)
		if status.RetryAfter != "" {
			w.Header().Set("Retry-After", status.RetryAfter)
		}
		code := "api_error"
		switch {
		case status.Code == http.StatusTooManyRequests:
			code = "rate_limit_error"
		case status.Code >= 401 && status.Code <= 403:
			code = "authentication_error"
		case status.Code == http.StatusNotFound:
			code = "not_found_error"
		}
		writeError(w, status.Code, code, status.Body)
		return
	}
	writeError(w, http.StatusBadGateway, "api_error", err.Error())
}

func writeError(w http.ResponseWriter, code int, kind, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"type": kind, "message": message}})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// copyStreamed relays an upstream body verbatim, flushing as it goes so SSE clients
// see tokens instead of one blob at the end.
func copyStreamed(w http.ResponseWriter, res *http.Response) {
	if ct := res.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(res.StatusCode)
	flusher, canFlush := w.(http.Flusher)
	if !canFlush || !strings.Contains(res.Header.Get("Content-Type"), "event-stream") {
		_, _ = io.Copy(w, res.Body)
		return
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 16<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintln(w, line); err != nil {
			return
		}
		if line == "" {
			flusher.Flush()
		}
	}
	flusher.Flush()
}
