package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// newLineScanner allows the large data frames some upstreams emit.
func newLineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 16<<10), 4<<20)
	return scanner
}

// usageFields is the subset of an OpenAI response we need for accounting. Both the
// non-stream object and the streamed trailer chunk share it.
type usageFields struct {
	Model string `json:"model"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (u *usageFields) tokens() (model string, in, out int) {
	if u == nil {
		return "", 0, 0
	}
	model = u.Model
	if u.Usage != nil {
		in, out = u.Usage.PromptTokens, u.Usage.CompletionTokens
	}
	return
}

// record notes one completed call. Tokens are whatever the upstream reported; a zero
// means it never sent usage, which the panel surfaces rather than hides.
func (s *Server) record(model string, in, out int, failed bool) {
	s.tracker.Record(model, in, out, failed)
}

// recordFromChatJSON accounts for a non-streaming chat completion body.
func (s *Server) recordFromChatJSON(raw []byte) {
	var fields usageFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return
	}
	model, in, out := fields.tokens()
	if model == "" {
		model = s.credential().Model
	}
	s.record(model, in, out, false)
}

// relayAndAccount forwards an upstream body verbatim while sniffing token usage out of
// it, so passthrough clients (Cursor, Cline) are counted too without buffering responses.
func relayAndAccount(w http.ResponseWriter, res *http.Response, fallbackModel string, onUsage func(model string, in, out int)) {
	streaming := strings.Contains(res.Header.Get("Content-Type"), "event-stream")
	if !streaming {
		body, err := io.ReadAll(res.Body)
		if err != nil {
			http.Error(w, "upstream read failed", http.StatusBadGateway)
			return
		}
		var fields usageFields
		_ = json.Unmarshal(body, &fields)
		model, in, out := fields.tokens()
		if model == "" {
			model = fallbackModel
		}
		onUsage(model, in, out)
		if ct := res.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(body)
		return
	}

	if ct := res.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(res.StatusCode)
	flusher, canFlush := w.(http.Flusher)
	relayLine := func(line string) {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
		if canFlush && line == "" {
			flusher.Flush()
		}
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" || !strings.Contains(payload, `"usage"`) {
			return
		}
		var fields usageFields
		if err := json.Unmarshal([]byte(payload), &fields); err != nil {
			return
		}
		if fields.Usage == nil {
			return
		}
		model, in, out := fields.tokens()
		if model == "" {
			model = fallbackModel
		}
		onUsage(model, in, out)
	}
	scanner := newLineScanner(res.Body)
	for scanner.Scan() {
		relayLine(strings.TrimRight(scanner.Text(), "\r"))
	}
	if canFlush {
		flusher.Flush()
	}
}
