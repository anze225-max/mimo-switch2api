package wire

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// Usage in OpenAI naming; Anthropic's input_tokens/output_tokens are derived from it.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// looseString absorbs the shapes upstreams actually send for string fields: a string,
// null, or an array of content parts.
type looseString string

func (s *looseString) UnmarshalJSON(raw []byte) error {
	text := string(raw)
	switch {
	case text == "", text == "null":
		*s = ""
	case text[0] == '"':
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		*s = looseString(v)
	case text[0] == '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return err
		}
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		*s = looseString(sb.String())
	default:
		*s = looseString(text)
	}
	return nil
}

func (s looseString) String() string { return string(s) }

type Chunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int    `json:"index"`
	Delta        Delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

type Delta struct {
	Role             string          `json:"role"`
	Content          looseString     `json:"content"`
	ReasoningContent looseString     `json:"reasoning_content"`
	ToolCalls        []DeltaToolCall `json:"tool_calls"`
}

type DeltaToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Event is one Anthropic SSE frame.
type Event struct {
	Type string
	Data any
}

const (
	blockText     = "text"
	blockToolUse  = "tool_use"
	blockThinking = "thinking"
)

// Stream turns an OpenAI token stream into Anthropic's event sequence. Anthropic
// clients expect opened/delta/closed blocks per content part; OpenAI has no such
// notion, so this owns the bookkeeping.
type Stream struct {
	id         string
	model      string
	started    bool
	nextIndex  int
	openIndex  int
	openKind   string
	toolBlocks map[int]int
	inputTok   int
	outputTok  int
	stopReason string
}

func NewStream() *Stream {
	return &Stream{id: newID("msg"), openIndex: -1, toolBlocks: map[int]int{}}
}

// Start is deliberately empty: message_start must carry the upstream's model and id,
// which are only known once the first chunk arrives, so the header is emitted lazily.
func (s *Stream) Start() []Event { return nil }

// header emits message_start exactly once.
func (s *Stream) header() []Event {
	if s.started {
		return nil
	}
	s.started = true
	return []Event{
		{Type: "message_start", Data: map[string]any{"type": "message_start", "message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "content": []any{},
			"model": s.model, "usage": map[string]any{"input_tokens": s.inputTok, "output_tokens": 1},
		}}},
		{Type: "ping", Data: map[string]any{"type": "ping"}},
	}
}

func (s *Stream) Feed(chunk Chunk) []Event {
	var events []Event
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.inputTok = chunk.Usage.PromptTokens
		s.outputTok = chunk.Usage.CompletionTokens
	}
	events = append(events, s.header()...)
	if len(chunk.Choices) == 0 {
		return events // usage-only trailer
	}
	choice := chunk.Choices[0]

	if r := choice.Delta.ReasoningContent.String(); r != "" {
		opened, idx := s.ensure(blockThinking)
		events = append(events, opened...)
		events = append(events, textDelta("thinking_delta", "thinking", idx, r))
	}
	if c := choice.Delta.Content.String(); c != "" {
		opened, idx := s.ensure(blockText)
		events = append(events, opened...)
		events = append(events, textDelta("text_delta", "text", idx, c))
	}
	for position, tc := range choice.Delta.ToolCalls {
		upstreamIndex := position
		if tc.Index != nil {
			upstreamIndex = *tc.Index
		}
		block, seen := s.toolBlocks[upstreamIndex]
		if !seen {
			events = append(events, s.closeOpen()...)
			block = s.nextIndex
			s.nextIndex++
			s.openIndex, s.openKind = block, blockToolUse
			s.toolBlocks[upstreamIndex] = block
			events = append(events, Event{Type: "content_block_start", Data: map[string]any{
				"type": "content_block_start", "index": block,
				"content_block": map[string]any{
					"type": blockToolUse, "id": toolUseID(tc.ID, upstreamIndex),
					"name": tc.Function.Name, "input": map[string]any{},
				},
			}})
		}
		if tc.Function.Arguments != "" {
			events = append(events, Event{Type: "content_block_delta", Data: map[string]any{
				"type": "content_block_delta", "index": block,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
			}})
		}
	}
	if choice.FinishReason != "" {
		s.stopReason = MapStopReason(choice.FinishReason)
		events = append(events, s.closeOpen()...)
	}
	return events
}

// ensure returns the frames needed to switch to (or stay on) a block of kind, plus
// its index.
func (s *Stream) ensure(kind string) ([]Event, int) {
	if s.openKind == kind && s.openIndex >= 0 {
		return nil, s.openIndex
	}
	events := s.closeOpen()
	index := s.nextIndex
	s.nextIndex++
	s.openIndex, s.openKind = index, kind
	events = append(events, Event{Type: "content_block_start", Data: map[string]any{
		"type": "content_block_start", "index": index, "content_block": startBlock(kind),
	}})
	return events, index
}

func startBlock(kind string) map[string]any {
	switch kind {
	case blockThinking:
		return map[string]any{"type": blockThinking, "thinking": ""}
	default:
		return map[string]any{"type": blockText, "text": ""}
	}
}

func (s *Stream) closeOpen() []Event {
	if s.openIndex < 0 {
		return nil
	}
	events := []Event{{Type: "content_block_stop", Data: map[string]any{
		"type": "content_block_stop", "index": s.openIndex,
	}}}
	s.openIndex, s.openKind = -1, ""
	return events
}

func textDelta(deltaKind, field string, index int, value string) Event {
	return Event{Type: "content_block_delta", Data: map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": deltaKind, field: value},
	}}
}

// Finish emits the terminal frames; call it even when the stream ended abruptly so
// clients see a well-formed close.
func (s *Stream) Finish() []Event {
	events := s.header()
	events = append(events, s.closeOpen()...)
	reason := s.stopReason
	if reason == "" {
		reason = "end_turn"
	}
	return append(events,
		Event{Type: "message_delta", Data: map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
			"usage": map[string]any{"input_tokens": s.inputTok, "output_tokens": atLeastOne(s.outputTok)},
		}},
		Event{Type: "message_stop", Data: map[string]any{"type": "message_stop"}})
}

// Usage reports the token counts the upstream itself returned for this stream.
func (s *Stream) Usage() (promptTokens, completionTokens int) {
	return s.inputTok, s.outputTok
}

// MapStopReason converts an OpenAI finish_reason onto Anthropic's vocabulary, which
// has no tool_calls value.
func MapStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default: // stop, content_filter, and anything unrecognised
		return "end_turn"
	}
}

// toolUseID passes the upstream id through verbatim. Rewriting it would force us to
// remember a mapping across requests, because the client echoes this id back in the
// next turn's tool_result and we must translate it into tool_call_id.
func toolUseID(id string, index int) string {
	if id != "" {
		return id
	}
	return "toolu_" + strconv.Itoa(index)
}

func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "_0"
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// ---------- non-streaming ----------

type Completion struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   Usage              `json:"usage"`
}

type CompletionChoice struct {
	Index        int     `json:"index"`
	Message      ChatMsg `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type ChatMsg struct {
	Role             string         `json:"role"`
	Content          looseString    `json:"content"`
	ReasoningContent looseString    `json:"reasoning_content"`
	ToolCalls        []ChatToolCall `json:"tool_calls"`
}

// CompletionToAnthropic renders a full OpenAI response as an Anthropic message object.
func CompletionToAnthropic(raw []byte) ([]byte, error) {
	var c Completion
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	blocks := []map[string]any{}
	reason := ""
	if len(c.Choices) > 0 {
		msg := c.Choices[0].Message
		// Reasoning first, mirroring Anthropic's thinking-then-text ordering. Dropping it
		// would blank out replies from models that answer partly by thinking.
		if think := msg.ReasoningContent.String(); think != "" {
			blocks = append(blocks, map[string]any{"type": blockThinking, "thinking": think})
		}
		if body := msg.Content.String(); body != "" {
			blocks = append(blocks, map[string]any{"type": blockText, "text": body})
		}
		for i, tc := range msg.ToolCalls {
			input := map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			blocks = append(blocks, map[string]any{
				"type": blockToolUse, "id": toolUseID(tc.ID, i), "name": tc.Function.Name, "input": input,
			})
		}
		reason = c.Choices[0].FinishReason
	}
	id := c.ID
	if id == "" {
		id = newID("msg")
	}
	return json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": c.Model,
		"content": blocks, "stop_reason": MapStopReason(reason), "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": c.Usage.PromptTokens, "output_tokens": c.Usage.CompletionTokens},
	})
}
