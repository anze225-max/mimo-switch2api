package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// product is the `kn` the platform uses to group issued keys.
const product = "mimocode"

// DefaultTimeout mirrors the desktop plugin's 5 minute authorization window.
const DefaultTimeout = 5 * time.Minute

func PlatformURL() string {
	if v := strings.TrimSpace(os.Getenv("MIMO_PLATFORM_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://platform.xiaomimimo.com"
}

// KeyName is stable per install because the platform lists issued keys by name;
// re-authorizing should renew rather than pile up entries in the user's dashboard.
func KeyName(dir string) (string, error) {
	path := filepath.Join(dir, "key-name")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("key name entropy: %w", err)
	}
	name := keyPrefix + hex.EncodeToString(buf[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// Session is one in-flight authorization: a loopback listener plus the ephemeral
// keypair whose public half the platform encrypts the key against.
type Session struct {
	kp           *KeyPair
	ln           net.Listener
	authorizeURL string
	manualURL    string
	result       chan outcome
}

type outcome struct {
	res *Result
	err error
}

func StartSession(dir string) (*Session, error) {
	kp, err := GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("loopback listener: %w", err)
	}
	base := PlatformURL()
	name, err := KeyName(dir)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	query := url.Values{}
	query.Set("pk", kp.PublicKeyParam())
	query.Set("redirect_uri", fmt.Sprintf("http://localhost:%d/", ln.Addr().(*net.TCPAddr).Port))
	query.Set("kn", product)
	query.Set("key_name", name)

	s := &Session{
		kp:           kp,
		ln:           ln,
		authorizeURL: base + "/authorize?" + query.Encode(),
		manualURL:    buildManualURL(base, kp, name),
		result:       make(chan outcome, 1),
	}
	go s.serve(base)
	return s, nil
}

func buildManualURL(base string, kp *KeyPair, name string) string {
	query := url.Values{}
	query.Set("pk", kp.PublicKeyParam())
	query.Set("redirect_uri", base+"/authorize/code/callback")
	query.Set("kn", product)
	query.Set("key_name", name)
	return base + "/authorize?" + query.Encode()
}

func (s *Session) AuthorizeURL() string { return s.authorizeURL }

// ManualURL is the fallback for when the loopback redirect cannot reach us, e.g. a
// remote or sandboxed browser: the page shows a code the user pastes back.
func (s *Session) ManualURL() string { return s.manualURL }

func (s *Session) OpenBrowser() error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", s.authorizeURL)
	case "darwin":
		cmd = exec.Command("open", s.authorizeURL)
	default:
		cmd = exec.Command("xdg-open", s.authorizeURL)
	}
	return cmd.Start()
}

func (s *Session) serve(base string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		encrypted := r.URL.Query().Get("u")
		if encrypted == "" {
			s.finish(outcome{err: errors.New("authorize callback carried no data")})
			redirectResult(w, r, base, "error", "missing_data")
			return
		}
		res, err := s.kp.Decrypt(encrypted)
		if err != nil {
			s.finish(outcome{err: err})
			redirectResult(w, r, base, "error", "decrypt_failed")
			return
		}
		s.finish(outcome{res: res})
		redirectResult(w, r, base, "success", "")
	})
	_ = http.Serve(s.ln, mux)
}

func redirectResult(w http.ResponseWriter, r *http.Request, base, status, message string) {
	target := base + "/authorize/callback?status=" + url.QueryEscape(status)
	if message != "" {
		target += "&message=" + url.QueryEscape(message)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// Wait blocks until the user finishes in the browser or ctx expires.
func (s *Session) Wait(ctx context.Context) (*Result, error) {
	select {
	case o := <-s.result:
		return o.res, o.err
	case <-ctx.Done():
		return nil, fmt.Errorf("authorization timed out: %w", ctx.Err())
	}
}

// Handcode completes a session from a copied code instead of the loopback redirect.
func (s *Session) Handcode(code string) (*Result, error) {
	res, err := s.kp.Decrypt(strings.TrimSpace(code))
	if err != nil {
		return nil, err
	}
	s.finish(outcome{res: res})
	return res, nil
}

func (s *Session) finish(o outcome) {
	select {
	case s.result <- o:
	default:
	}
}

func (s *Session) Close() error { return s.ln.Close() }
