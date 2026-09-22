// Package store persists the tool's config and the issued MiMo credential.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kind distinguishes the two upstreams this tool can front: a platform API key issued by
// the official authorize flow, or a harvested MiMo desktop session (free quota).
const (
	KindPlatform = "platform"
	KindDesktop  = "desktop"
)

type Credential struct {
	SK      string `json:"sk"`
	Cookie  string `json:"cookie,omitempty"`
	Kind    string `json:"kind,omitempty"`
	BaseURL string `json:"base_url"`
	UID     string `json:"uid"`
	KeyName string `json:"key_name"`
	Model   string `json:"model,omitempty"`

	// Master credential for the desktop session: passport's long-lived passToken lets the
	// tool renew serviceToken on its own, with MiMo closed. Kept inside the DPAPI blob.
	PassToken string `json:"pass_token,omitempty"`
	CUserId   string `json:"c_user_id,omitempty"`

	IssuedAt time.Time `json:"issued_at"`
}

// Plan reports which quota this credential draws on.
func (c *Credential) Plan() string {
	if c == nil {
		return ""
	}
	if c.Kind == KindDesktop {
		return "desktop-free"
	}
	if strings.Contains(c.BaseURL, "token-plan") {
		return "token-plan"
	}
	return "billing"
}

type Config struct {
	Listen       string `json:"listen"`
	LocalToken   string ` json:"local_token"`
	RequireToken bool   `json:"require_token"`

	// ModelAliases maps a client's model label (e.g. "sol") onto a real MiMo model id.
	// Empty means use the built-in defaults.
	ModelAliases map[string]string `json:"model_aliases,omitempty"`

	// Protected is base64(DPAPI(credential JSON)). The secret never sits in the
	// config file in plaintext, but is readable to this user without a passphrase.
	Protected string `json:"credential_protected,omitempty"`

	cred *Credential
}

// Masked is the only rendering of the secret this program prints or logs.
func (c *Credential) Masked() string {
	secret := c.SK
	if c.Kind == KindDesktop {
		secret = c.Cookie
	}
	if len(secret) < 14 {
		return "已配置"
	}
	return secret[:8] + "…" + secret[len(secret)-4:] + fmt.Sprintf(" (%d)", len(secret))
}

// Fingerprint is a short hash of the secret. Masked() cannot show whether a session cookie
// was renewed, because it is dominated by the constant "serviceToken=" prefix and the
// trailing userId; this can.
func (c *Credential) Fingerprint() string {
	secret := c.SK
	if c.Kind == KindDesktop {
		secret = c.Cookie
	}
	if secret == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:10]
}

func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "mimo-switch")
	return dir, os.MkdirAll(dir, 0o700)
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load returns the config, creating an in-memory default with a fresh local token
// when none exists yet. Saving is the caller's decision.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	cfg := &Config{Listen: "127.0.0.1:7864", RequireToken: true}
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		if cfg.LocalToken, err = randomToken(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if cfg.Protected != "" {
		blob, err := base64.StdEncoding.DecodeString(cfg.Protected)
		if err != nil {
			return nil, fmt.Errorf("credential blob: %w", err)
		}
		plain, err := unprotect(blob)
		if err != nil {
			return nil, fmt.Errorf("unlock credential (re-run authorize if this machine changed user): %w", err)
		}
		var c Credential
		if err := json.Unmarshal(plain, &c); err != nil {
			return nil, fmt.Errorf("credential json: %w", err)
		}
		cfg.cred = &c
	}
	return cfg, nil
}

func (c *Config) Credential() *Credential { return c.cred }

func (c *Config) SetCredential(cred *Credential) { c.cred = cred }

func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	out := *c
	if c.cred != nil {
		plain, err := json.Marshal(c.cred)
		if err != nil {
			return err
		}
		blob, err := protect(plain)
		if err != nil {
			return err
		}
		out.Protected = base64.StdEncoding.EncodeToString(blob)
	}
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
