package wire

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The Responses API (used by Codex) is a third shape on top of Chat Completions:
// history is "input items" instead of messages, and tool calls are function_call /
// function_call_output items rather than role:"tool" turns.

type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    json.RawMessage `json:"instructions,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Tools           []ChatTool      `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
}

// ResponsesToChat turns a /v1/responses request into a /v1/chat/completions request.
func ResponsesToChat(body []byte) ([]byte, error) {
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	out := ChatRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}
	if out.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if sys := rawTextOrParts(req.Instructions); sys != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: sys})
	}

	items, err := inputItems(req.Input)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		switch item.Type {
		case "function_call":
			out.Messages = append(out.Messages, ChatMessage{
				Role: "assistant",
				ToolCalls: []ChatToolCall{{
					ID:       firstNonEmpty(item.CallID, item.ID),
					Type:     "function",
					Function: ChatFunction{Name: item.Name, Arguments: item.Arguments},
				}},
			})
		case "function_call_output":
			out.Messages = append(out.Messages, ChatMessage{
				Role: "tool", ToolCallID: item.CallID, Content: rawTextOrParts(item.Output),
			})
		default:
			parts := responsesContent(item.Content)
			if len(parts) == 0 {
				continue
			}
			var content any
			if len(parts) == 1 && parts[0]["type"] == "text" {
				content = parts[0]["text"]
			} else {
				content = parts
			}
			out.Messages = append(out.Messages, ChatMessage{Role: firstNonEmpty(item.Role, "user"), Content: content})
		}
	}
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("responses input produced no messages")
	}
	return json.Marshal(out)
}

type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// inputItems accepts both documented shapes of `input`: a bare string, and an item array.
func inputItems(raw json.RawMessage) ([]inputItem, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("responses request has no input")
	}
	var as string
	if err := json.Unmarshal(raw, &as); err == nil {
		return []inputItem{{Type: "message", Role: "user", Content: raw}}, nil
	}
	var items []inputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses input: %w", err)
	}
	return items, nil
}

// responsesContent converts a Responses content array into OpenAI chat content parts,
// carrying input_image through so Codex screenshots survive the hop.
func responsesContent(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var bare string
	if err := json.Unmarshal(raw, &bare); err == nil {
		if bare == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": bare}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			if p.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": p.Text})
			}
		case "input_image", "image_url":
			if p.ImageURL == "" {
				continue
			}
			image := map[string]any{"url": p.ImageURL}
			if p.Detail != "" {
				image["detail"] = p.Detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// rawTextOrParts flattens a field that may be a string, a {text} object, or a content
// part array.
func rawTextOrParts(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bare string
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	var single struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &single); err == nil {
		return single.Text
	}
	return ""
}

// ChatToResponse renders a chat completion as a Responses output object.
func ChatToResponse(chatBody []byte) ([]byte, error) {
	var c Completion
	if err := json.Unmarshal(chatBody, &c); err != nil {
		return nil, err
	}
	// output must always be an array: clients treat null as a malformed response.
	items := []map[string]any{}
	text := ""
	if len(c.Choices) > 0 {
		msg := c.Choices[0].Message
		if think := msg.ReasoningContent.String(); think != "" {
			items = append(items, map[string]any{
				"type": "reasoning", "id": newID("rs"), "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "reasoning_text", "text": think}},
			})
		}
		if body := msg.Content.String(); body != "" {
			text = body
			items = append(items, map[string]any{
				"type": "message", "id": newID("msg"), "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": body, "annotations": []any{}}},
			})
		}
		for _, tc := range msg.ToolCalls {
			items = append(items, map[string]any{
				"type": "function_call", "id": newID("fc"), "call_id": tc.ID,
				"name": tc.Function.Name, "arguments": tc.Function.Arguments, "status": "completed",
			})
		}
	}
	return json.Marshal(map[string]any{
		"id": newID("resp"), "object": "response", "status": "completed",
		"model": c.Model, "output": items, "output_text": text,
		"usage": map[string]any{
			"input_tokens": c.Usage.PromptTokens, "output_tokens": c.Usage.CompletionTokens,
			"total_tokens": c.Usage.PromptTokens + c.Usage.CompletionTokens,
		},
	})
}

// ResponseStream turns a chat SSE stream into Responses SSE events.
type ResponseStream struct {
	responseID string
	started    bool

	text       strings.Builder
	textItemID string
	textIndex  int

	reasoning       strings.Builder
	reasoningItemID string
	reasoningIndex  int

	tools     map[int]*respTool
	toolOrder []int
	items     []map[string]any

	inputTok  int
	outputTok int
}

type respTool struct {
	itemID, callID, name string
	index                int
	args                 strings.Builder
}

func NewResponseStream() *ResponseStream {
	return &ResponseStream{responseID: newID("resp"), tools: map[int]*respTool{}}
}

func (s *ResponseStream) Start() []Event {
	if s.started {
		return nil
	}
	s.started = true
	shell := map[string]any{"id": s.responseID, "object": "response", "status": "in_progress", "output": []any{}}
	return []Event{
		{Type: "response.created", Data: map[string]any{"type": "response.created", "response": shell}},
		{Type: "response.in_progress", Data: map[string]any{"type": "response.in_progress", "response": shell}},
	}
}

func (s *ResponseStream) Feed(chunk Chunk) []Event {
	var events []Event
	if chunk.Usage != nil {
		s.inputTok = chunk.Usage.PromptTokens
		s.outputTok = chunk.Usage.CompletionTokens
	}
	if len(chunk.Choices) == 0 {
		return events
	}
	delta := chunk.Choices[0].Delta

	if think := delta.ReasoningContent.String(); think != "" {
		if s.reasoningItemID == "" {
			s.reasoningItemID = newID("rs")
			s.reasoningIndex = len(s.items)
			s.items = append(s.items, map[string]any{"type": "reasoning", "id": s.reasoningItemID})
			events = append(events, Event{Type: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "output_index": s.reasoningIndex,
				"item": map[string]any{"type": "reasoning", "id": s.reasoningItemID, "role": "assistant"},
			}})
		}
		s.reasoning.WriteString(think)
		events = append(events, Event{Type: "response.reasoning_text.delta", Data: map[string]any{
			"type": "response.reasoning_text.delta", "item_id": s.reasoningItemID,
			"output_index": s.reasoningIndex, "content_index": 0, "delta": think,
		}})
	}

	if text := delta.Content.String(); text != "" {
		if s.textItemID == "" {
			s.textItemID = newID("msg")
			s.textIndex = len(s.items)
			s.items = append(s.items, map[string]any{"type": "message", "id": s.textItemID, "role": "assistant"})
			events = append(events,
				Event{Type: "response.output_item.added", Data: map[string]any{
					"type": "response.output_item.added", "output_index": s.textIndex,
					"item": map[string]any{"type": "message", "id": s.textItemID, "role": "assistant", "content": []any{}},
				}},
				Event{Type: "response.content_part.added", Data: map[string]any{
					"type": "response.content_part.added", "item_id": s.textItemID,
					"output_index": s.textIndex, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				}})
		}
		s.text.WriteString(text)
		events = append(events, Event{Type: "response.output_text.delta", Data: map[string]any{
			"type": "response.output_text.delta", "item_id": s.textItemID,
			"output_index": s.textIndex, "content_index": 0, "delta": text,
		}})
	}

	for position, tc := range delta.ToolCalls {
		index := position
		if tc.Index != nil {
			index = *tc.Index
		}
		block, seen := s.tools[index]
		if !seen {
			block = &respTool{
				itemID: newID("fc"),
				callID: firstNonEmpty(tc.ID, "call_"+strconv.Itoa(index)),
				name:   tc.Function.Name,
				index:  len(s.items),
			}
			s.tools[index] = block
			s.toolOrder = append(s.toolOrder, index)
			s.items = append(s.items, map[string]any{"type": "function_call", "id": block.itemID})
			events = append(events, Event{Type: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "output_index": block.index,
				"item": map[string]any{"type": "function_call", "id": block.itemID, "call_id": block.callID, "name": block.name},
			}})
		}
		if block.name == "" && tc.Function.Name != "" {
			block.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			block.args.WriteString(tc.Function.Arguments)
			events = append(events, Event{Type: "response.function_call_arguments.delta", Data: map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": block.itemID,
				"output_index": block.index, "delta": tc.Function.Arguments,
			}})
		}
	}
	return events
}

// Usage reports the token counts the upstream itself returned for this stream.
func (s *ResponseStream) Usage() (promptTokens, completionTokens int) {
	return s.inputTok, s.outputTok
}

func (s *ResponseStream) Finish() []Event {
	var events []Event
	if s.reasoningItemID != "" {
		events = append(events, Event{Type: "response.output_item.done", Data: map[string]any{
			"type": "response.output_item.done", "output_index": s.reasoningIndex,
			"item": map[string]any{
				"type": "reasoning", "id": s.reasoningItemID, "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "reasoning_text", "text": s.reasoning.String()}},
			},
		}})
	}
	if s.textItemID != "" {
		events = append(events,
			Event{Type: "response.output_text.done", Data: map[string]any{
				"type": "response.output_text.done", "item_id": s.textItemID, "text": s.text.String(),
			}},
			Event{Type: "response.output_item.done", Data: map[string]any{
				"type": "response.output_item.done", "output_index": s.textIndex,
				"item": map[string]any{
					"type": "message", "id": s.textItemID, "role": "assistant", "status": "completed",
					"content": []map[string]any{{"type": "output_text", "text": s.text.String(), "annotations": []any{}}},
				},
			}})
	}
	for _, key := range s.toolOrder {
		block := s.tools[key]
		events = append(events, Event{Type: "response.output_item.done", Data: map[string]any{
			"type": "response.output_item.done", "output_index": block.index,
			"item": map[string]any{
				"type": "function_call", "id": block.itemID, "call_id": block.callID,
				"name": block.name, "arguments": block.args.String(), "status": "completed",
			},
		}})
	}
	events = append(events, Event{Type: "response.completed", Data: map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": s.responseID, "object": "response", "status": "completed", "output": s.items,
			"usage": map[string]any{
				"input_tokens": s.inputTok, "output_tokens": s.outputTok,
				"total_tokens": s.inputTok + s.outputTok,
			},
		},
	}})
	return events
}
