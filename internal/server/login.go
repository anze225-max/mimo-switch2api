package server

import (
	"context"
	"errors"
	"fmt"
	"os"

	"mimo-switch/internal/login"
)

// ErrLoginBusy means a login window is already open; a second one would only confuse the
// user about which window to sign in to.
var ErrLoginBusy = errors.New("已有登录窗口等待你完成登录")

// StartLogin opens the passport window and adopts the session the user signs in for, so the
// tray and the panel can re-authorise without restarting the proxy. It returns once the
// window is up; the wait happens in the background.
func (s *Server) StartLogin() error {
	if !s.loginBusy.CompareAndSwap(false, true) {
		return ErrLoginBusy
	}
	sess, err := login.Start()
	if err != nil {
		s.loginBusy.Store(false)
		return err
	}
	go func() {
		defer s.loginBusy.Store(false)
		defer sess.Close()

		ctx, cancel := context.WithTimeout(context.Background(), login.DefaultTimeout)
		defer cancel()
		session, err := sess.Wait(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "登录未完成: %v\n", err)
			return
		}
		if err := s.AdoptDesktop(session); err != nil {
			fmt.Fprintf(os.Stderr, "登录成功但会话未被接受: %v\n", err)
		}
	}()
	return nil
}

// LoginPending reports whether a login window is waiting on the user.
func (s *Server) LoginPending() bool { return s.loginBusy.Load() }
