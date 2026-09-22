package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"mimo-switch/internal/desktopauth"
	"mimo-switch/internal/store"
	"mimo-switch/internal/upstream"
	"mimo-switch/internal/usage"
)

// refreshCooldown stops a burst of concurrent 401s from each firing a passport renewal.
const refreshCooldown = 10 * time.Second

var errNoMaster = errors.New("没有保存 passToken，无法自动续期；请重新运行 mimo-switch login")

// relogin is the single seam for renewing the session: it returns a live session plus the
// model verified against it. Tests substitute it; production uses MiMo's passport flow.
type relogin func() (*desktopauth.Session, string, error)

// catalogueCandidates republishes MiMo's model list (it can change while we run) and returns
// the ids to verify a session against. With MiMo uninstalled the list is empty and Verify
// falls back to its own short list, so a login still works on a clean machine.
func (s *Server) catalogueCandidates() []string {
	models := s.catalogue()
	if fresh, err := usage.LoadTextModels(); err == nil {
		models = fresh
		s.tracker.SetMultipliers(fresh)
	}
	s.setSession(s.credential(), models)
	s.rebuildResolver()
	return s.catalogIDs()
}

func (s *Server) defaultRelogin() (*desktopauth.Session, string, error) {
	master := desktopauth.MasterCredential{
		PassToken: s.credential().PassToken,
		UserID:    s.credential().UID,
		CUserId:   s.credential().CUserId,
		SID:       desktopauth.DefaultSID,
	}
	session, err := desktopauth.Refresh(master)
	if err != nil {
		return nil, "", err
	}
	model, err := session.Verify(s.catalogueCandidates()...)
	if err != nil {
		return nil, "", err
	}
	return session, model, nil
}

// applySession makes a live desktop session the server's own: it publishes a new credential
// copy (handlers keep the previous one and must never see a half-written struct), points the
// upstream client at it, and persists it. MiMo does not need to be running.
func (s *Server) applySession(session *desktopauth.Session, model string) {
	next := *s.credential()
	if session.UserID != "" {
		next.UID = session.UserID
	}
	if session.PassToken != "" {
		next.PassToken = session.PassToken
	}
	if session.CUserId != "" {
		next.CUserId = session.CUserId
	}
	next.Kind = store.KindDesktop
	next.BaseURL = s.desktopEndpoint()
	next.Cookie = session.CookieHeader()
	next.Model = model
	next.IssuedAt = session.HarvestedAt.UTC()

	s.setSession(&next, nil)
	s.rebuildResolver() // the default model clients fall back to just changed
	s.client.SetCookie(next.Cookie)
	s.client.SetBaseURL(next.BaseURL)
	s.refreshedAt = time.Now()

	if s.cfg != nil {
		s.cfg.SetCredential(&next)
		if err := s.cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "会话已换取但保存凭证失败: %v\n", err)
		}
	}
}

// desktopVerify probes a session against the catalogue; tests substitute it because the real
// one posts to MiMo.
type desktopVerify func(session *desktopauth.Session) (string, error)

// desktopEndpoint is where an adopted desktop session points. Tests override it so no
// request ever leaves the machine.
func (s *Server) desktopEndpoint() string {
	if s.desktopBase != "" {
		return s.desktopBase
	}
	return desktopauth.BaseURL
}

// AdoptDesktop verifies a session the user just signed in for and installs it without a
// restart. This is the tray/panel "登录" path.
func (s *Server) AdoptDesktop(session *desktopauth.Session) error {
	verify := s.defaultVerify
	if s.verify != nil {
		verify = s.verify
	}
	model, err := verify(session)
	if err != nil {
		return err
	}
	s.applySession(session, model)
	fmt.Fprintf(os.Stderr, "已接入新的 MiMo 桌面端会话（模型 %s）\n", model)
	return nil
}

func (s *Server) defaultVerify(session *desktopauth.Session) (string, error) {
	return session.Verify(s.catalogueCandidates()...)
}

// refreshSession mints a new serviceToken from the stored passToken, swaps it into the live
// client, and persists it. MiMo does not need to be running.
//
// The lock is held across the whole renewal so parallel requests that all see a 401 result
// in exactly one passport round trip.
func (s *Server) refreshSession(ctx context.Context) error {
	if !s.canRefresh() {
		return errNoMaster
	}
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()
	if time.Since(s.refreshedAt) < refreshCooldown {
		return nil // a peer already renewed; retry with the current cookie
	}

	reloginFn := s.relogin
	if reloginFn == nil {
		reloginFn = s.defaultRelogin
	}
	session, model, err := reloginFn()
	if err != nil {
		return err
	}
	s.applySession(session, model)
	fmt.Fprintf(os.Stderr, "已自动续期 MiMo 会话（模型 %s）\n", model)
	return nil
}

func (s *Server) canRefresh() bool {
	return s.credential().Kind == store.KindDesktop && s.credential().PassToken != ""
}

// post sends to the upstream, renewing once and retrying if the session had expired.
func (s *Server) post(ctx context.Context, path string, body any) (*http.Response, error) {
	res, err := s.client.Post(ctx, path, body)
	if err == nil {
		return res, nil
	}
	if s.renewIfExpired(err) {
		return s.client.Post(ctx, path, body)
	}
	return nil, err
}

// renewIfExpired reports whether a retry is worthwhile after an upstream error.
func (s *Server) renewIfExpired(err error) bool {
	var status *upstream.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusUnauthorized {
		return false
	}
	if !s.canRefresh() {
		return false
	}
	if refreshErr := s.refreshSession(context.Background()); refreshErr != nil {
		fmt.Fprintf(os.Stderr, "自动续期失败: %v\n", refreshErr)
		return false
	}
	return true
}

// RefreshNow performs a renewal, used by the CLI and the tray menu.
func (s *Server) RefreshNow() error { return s.refreshSession(context.Background()) }
