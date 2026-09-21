package wire

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteEvent emits one Anthropic SSE frame.
func WriteEvent(w io.Writer, ev Event) error {
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
	return err
}

// WriteJSONEvent emits a frame whose data is already encoded, avoiding a re-marshal of
// payloads we are only forwarding.
func WriteJSONEvent(w io.Writer, eventType string, raw json.RawMessage) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw)
	return err
}

// OpenAILine parses one line of an upstream SSE stream. It reports done=true on the
// terminal [DONE] sentinel, and ok=false for anything that is not a data payload.
func OpenAILine(line string) (payload string, done bool, ok bool) {
	line = strings.TrimRight(line, "\r")
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return "", false, false
	case !strings.HasPrefix(trimmed, "data:"):
		return "", false, false
	}
	body := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if body == "[DONE]" {
		return "", true, false
	}
	return body, false, true
}
