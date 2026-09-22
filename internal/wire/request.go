// Package wire translates between the Anthropic Messages API and the OpenAI
// Chat Completions API. It is deliberately pure: no IO, no clock, no globals, so
// every mapping can be pinned with a fixture test.
package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMaxTokens is injected when an Anthropic client omits max_tokens, which is
// legal for the OpenAI side but not for the Anthropic side.
const DefaultMaxTokens = 8192

// ---------- Anthropic request ----------

type Request struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
}

type Message struct {
	Role string `json:"role"`
	// Content is either a bare string or an array of blocks; kept raw so both
	// shapes survive without a bespoke unmarshaller.
	Content json.RawMessage `json:"content"`
}

type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *ImageSource    `json:"source,omitempty"`
}

// ImageSource is Anthropic's image payload wrapper: either inline base64 or a URL.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ---------- OpenAI request ----------

type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	Tools         []ChatTool     `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
}

// StreamOptions is set so the upstream emits a final usage chunk; without it the
// quota panel and Anthropic's message_delta would have to estimate.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
}

type ChatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

type ChatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatToolSpec `json:"function"`
}

type ChatToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// AnthropicToOpenAI rewrites a /v1/messages body into a /v1/chat/completions body.
func AnthropicToOpenAI(body []byte) ([]byte, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	out := ChatRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
		Stream:      req.Stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}
	if out.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if text := systemText(req.System); text != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: text})
	}
	for _, m := range req.Messages {
		blocks, err := m.blocks()
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, messageToOpenAI(m.Role, blocks)...)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ChatTool{Type: "function", Function: ChatToolSpec{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
	}
	choice, err := toolChoiceToOpenAI(req.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = choice
	return json.Marshal(out)
}

// messageToOpenAI expands one Anthropic message into the OpenAI messages it implies,
// because a single Anthropic user turn can carry several tool results that OpenAI
// models as separate role:"tool" messages.
//
// Anthropic's model picker advertises TEXT only, but mimo-v2.6 accepts and understands
// images (verified against the live endpoint), so image blocks are carried across rather
// than dropped. Document blocks remain unsupported.
func messageToOpenAI(role string, blocks []Block) []ChatMessage {
	var (
		text       strings.Builder
		calls      []ChatToolCall
		tools      []ChatMessage
		images     []map[string]any
		toolImages []map[string]any
	)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, ChatToolCall{
				ID: b.ID, Type: "function",
				Function: ChatFunction{Name: b.Name, Arguments: string(b.Input)},
			})
		case "tool_result":
			body, pics := toolResultContent(b)
			tools = append(tools, ChatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: body})
			toolImages = append(toolImages, pics...)
		case "image":
			if url, ok := imageURL(b.Source); ok {
				images = append(images, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		}
	}
	images = append(images, toolImages...)

	out := tools // tool results must land before any prose in the same turn
	if len(calls) > 0 {
		out = append(out, ChatMessage{Role: "assistant", Content: nilOrString(text.String()), ToolCalls: calls})
	} else {
		content := multimodalContent(text.String(), images)
		if content != nil || role != "assistant" {
			out = append(out, ChatMessage{Role: role, Content: content})
		}
		return out
	}
	if len(toolImages) > 0 {
		out = append(out, ChatMessage{Role: "user", Content: multimodalContent("", toolImages)})
	}
	return out
}

// multimodalContent returns OpenAI's content-part array when images are present, the
// bare string when they are not, and nil for an empty turn.
func multimodalContent(text string, images []map[string]any) any {
	if len(images) == 0 {
		return nilOrString(text)
	}
	parts := make([]map[string]any, 0, len(images)+1)
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	parts = append(parts, images...)
	return parts
}

// imageURL converts an Anthropic image source into a data: URL or plain URL that the
// OpenAI-shaped endpoint accepts.
func imageURL(src *ImageSource) (string, bool) {
	if src == nil {
		return "", false
	}
	switch src.Type {
	case "base64":
		if src.Data == "" || src.MediaType == "" {
			return "", false
		}
		return "data:" + src.MediaType + ";base64," + src.Data, true
	case "url":
		if src.URL == "" {
			return "", false
		}
		return src.URL, true
	}
	return "", false
}

func nilOrString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (m Message) blocks() ([]Block, error) {
	var as string
	if err := json.Unmarshal(m.Content, &as); err == nil {
		return []Block{{Type: "text", Text: as}}, nil
	}
	var blocks []Block
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("message content for role %s: %w", m.Role, err)
	}
	return blocks, nil
}

// toolResultContent flattens a tool_result into the text the upstream role:"tool"
// message carries, plus any pictures it wrapped — reading a file with Claude Code's Read
// tool returns a screenshot-shaped image block, and dropping it would blind the model.
func toolResultContent(b Block) (string, []map[string]any) {
	text, images := "", []map[string]any(nil)
	switch {
	case len(b.Content) == 0:
	case json.Unmarshal(b.Content, &text) == nil:
	default:
		var blocks []Block
		if err := json.Unmarshal(b.Content, &blocks); err != nil {
			return string(b.Content), nil
		}
		var sb strings.Builder
		for _, inner := range blocks {
			switch inner.Type {
			case "text":
				sb.WriteString(inner.Text)
			case "image":
				if url, ok := imageURL(inner.Source); ok {
					images = append(images, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		text = sb.String()
	}
	if b.IsError {
		text = "Error: " + text
	}
	return text, images
}

func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bare string
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

func toolChoiceToOpenAI(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var as string
	if err := json.Unmarshal(raw, &as); err == nil {
		return as, nil
	}
	var shape struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil, fmt.Errorf("tool_choice: %w", err)
	}
	switch shape.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": shape.Name}}, nil
	default:
		return nil, fmt.Errorf("unsupported tool_choice type %q", shape.Type)
	}
}
