// Package upstream talks to the MiMo OpenAI-compatible endpoint.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	sk      string
	cookie  string
	baseURL string
	http    *http.Client
}

func New(sk, baseURL string) *Client {
	return &Client{
		sk:      sk,
		baseURL: strings.TrimRight(baseURL, "/"),
		// Streaming responses must not be cut off by a transport-wide timeout; the
		// per-request context governs liveness instead.
		http: &http.Client{},
	}
}

// NewCookie builds a client for the desktop endpoint, which authenticates with a
// serviceToken cookie rather than a bearer key.
func NewCookie(cookieHeader, baseURL string) *Client {
	c := New("", baseURL)
	c.cookie = cookieHeader
	return c
}

func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) authorize(req *http.Request) {
	if c.cookie != "" {
		req.Header.Set("cookie", c.cookie)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.sk)
}

// StatusError carries the upstream status so the proxy can decide between
// "key is dead, re-authorize" and "you are rate limited, back off".
type StatusError struct {
	Code       int
	Body       string
	RetryAfter string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Code, truncate(e.Body, 240))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Post sends a JSON body to a path under the base URL and hands back the live
// response body for streaming. Callers must Close it.
func (c *Client) Post(ctx context.Context, path string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	return c.do(req)
}

func (c *Client) Get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	return c.do(req)
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		_ = res.Body.Close()
		return nil, &StatusError{Code: res.StatusCode, Body: string(body), RetryAfter: res.Header.Get("Retry-After")}
	}
	return res, nil
}

// Models returns the model ids the credential can actually reach, which is the
// cheapest proof that a freshly issued key works.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := c.Get(ctx, "/models")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// Chat fires a non-streaming completion, used by `probe` and the panel's test button.
func (c *Client) Chat(ctx context.Context, body map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := c.Post(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode chat completion: %w", err)
	}
	return out, nil
}
