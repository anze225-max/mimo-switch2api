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
	// The catalogue may have moved on since startup, so re-read it before verifying.
	var models []usage.CatalogModel
	if fresh, err := usage.LoadTextModels(); err == nil {
		models = fresh
		s.tracker.SetMultipliers(fresh)
	} else {
		models = s.catalogue()
	}
	candidates := make([]string, 0, len(models))
	for _, m := range models {
		candidates = append(candidates, m.ID)
	}
	// Publish the refreshed catalogue (and the untouched session) so the panel and the
	// model alias resolver see MiMo's current generation, not the one from startup.
	s.setSession(s.credential(), models)
	model, err := session.Verify(candidates...)
	if err != nil {
		return nil, "", err
	}
	return session, model, nil
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

	// Publish a copy: handlers hold the previous credential and must never observe a
	// half-written one.
	next := *s.credential()
	next.Cookie = session.CookieHeader()
	next.Model = model
	next.IssuedAt = session.HarvestedAt.UTC()
	s.setSession(&next, nil)
	s.client.SetCookie(next.Cookie)
	s.refreshedAt = time.Now()

	if s.cfg != nil {
		s.cfg.SetCredential(&next)
		if err := s.cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "续期成功但保存凭证失败: %v\n", err)
		}
	}
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
