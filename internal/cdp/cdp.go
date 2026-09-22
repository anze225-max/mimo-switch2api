// Package cdp is a minimal client for the Chrome/Node inspector protocol, used only to
// read the MiMo desktop app's own cookie jar over its local debug port.
package cdp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type target struct {
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Conn is one debugger session.
type Conn struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	next   int
	waiter map[int]chan json.RawMessage
}

// Connect opens a debugger session against the "node" target if present (the main
// process), else the first page target. Both expose Runtime.evaluate; only the node
// target can require('electron').
func Connect(port int) (*Conn, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("调试端口 %d 不可达（目标进程是否以调试端口启动？）: %w", port, err)
	}
	defer res.Body.Close()
	var targets []target
	if err := json.NewDecoder(res.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("解析调试目标: %w", err)
	}
	chosen := pickTarget(targets)
	if chosen == nil {
		return nil, fmt.Errorf("端口 %d 上没有可用的调试目标", port)
	}
	ws, _, err := websocket.DefaultDialer.Dial(chosen.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("连接调试会话: %w", err)
	}
	c := &Conn{ws: ws, waiter: map[int]chan json.RawMessage{}}
	go c.read()
	return c, nil
}

func pickTarget(targets []target) *target {
	for i := range targets {
		if targets[i].Type == "node" && targets[i].WebSocketDebuggerURL != "" {
			return &targets[i]
		}
	}
	for i := range targets {
		if targets[i].WebSocketDebuggerURL != "" {
			return &targets[i]
		}
	}
	return nil
}

func (c *Conn) read() {
	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch, ok := c.waiter[msg.ID]
		if ok {
			delete(c.waiter, msg.ID) // replies must not accumulate waiters either
		}
		c.mu.Unlock()
		if !ok {
			continue
		}
		if len(msg.Error) > 0 {
			ch <- msg.Error
			continue
		}
		ch <- msg.Result
	}
}

func (c *Conn) Call(method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan json.RawMessage, 1)
	c.waiter[id] = ch
	c.mu.Unlock()

	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	if err := c.ws.WriteJSON(payload); err != nil {
		return nil, err
	}
	select {
	case raw := <-ch:
		return raw, nil
	case <-time.After(20 * time.Second):
		// Without this the waiter channel stays registered forever, so every timed-out
		// call leaks an entry on a connection that may run for hours.
		c.mu.Lock()
		delete(c.waiter, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s 超时", method)
	}
}

// Evaluate runs an async expression and returns its string result. includeCommandLineAPI
// is required: Electron's packaged main process does not expose `require` to eval scopes
// without it.
func (c *Conn) Evaluate(expression string) (string, error) {
	if _, err := c.Call("Runtime.enable", nil); err != nil {
		return "", err
	}
	raw, err := c.Call("Runtime.evaluate", map[string]any{
		"expression":            "(async () => { " + expression + " })()",
		"returnByValue":         true,
		"awaitPromise":          true,
		"includeCommandLineAPI": true,
	})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails struct {
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("解析求值结果: %w (%s)", err, strings.TrimSpace(string(raw)))
	}
	if parsed.ExceptionDetails.Exception.Description != "" {
		return "", fmt.Errorf("主进程求值失败: %s", firstLine(parsed.ExceptionDetails.Exception.Description))
	}
	return parsed.Result.Value, nil
}

func (c *Conn) Close() error { return c.ws.Close() }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
