package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func translate(t *testing.T, body string) map[string]any {
	t.Helper()
	out, err := AnthropicToOpenAI([]byte(body))
	if err != nil {
		t.Fatalf("AnthropicToOpenAI: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestTranslatesTextTurnWithSystem(t *testing.T) {
	parsed := translate(t, `{
	  "model":"mimo-v2.5-pro","max_tokens":128,
	  "system":"You are terse.",
	  "messages":[{"role":"user","content":"hi"}]}`)

	messages := parsed["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("want system+user, got %d: %s", len(messages), mustJSON(t, messages))
	}
	if messages[0].(map[string]any)["role"] != "system" {
		t.Errorf("first message role = %v", messages[0])
	}
	if messages[1].(map[string]any)["content"] != "hi" {
		t.Errorf("user content = %v", messages[1])
	}
	if parsed["max_tokens"].(float64) != 128 {
		t.Errorf("max_tokens = %v", parsed["max_tokens"])
	}
}

func TestSystemAsBlockArray(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,
	  "system":[{"type":"text","text":"A. "},{"type":"text","text":"B."}],
	  "messages":[{"role":"user","content":"x"}]}`)
	first := parsed["messages"].([]any)[0].(map[string]any)
	if first["content"] != "A. B." {
		t.Errorf("system blocks = %v", first["content"])
	}
}

func TestToolUseBecomesAssistantToolCalls(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,"messages":[
	  {"role":"assistant","content":[
	     {"type":"text","text":"let me check"},
	     {"type":"tool_use","id":"call_7","name":"read","input":{"path":"a.txt"}}]}]}`)

	msg := parsed["messages"].([]any)[0].(map[string]any)
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", msg["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_7" {
		t.Errorf("id must pass through verbatim, got %v", call["id"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Errorf("name = %v", fn["name"])
	}
	if fn["arguments"] != `{"path":"a.txt"}` {
		t.Errorf("arguments must be a JSON string, got %v", fn["arguments"])
	}
	if msg["content"] != "let me check" {
		t.Errorf("text alongside tool_use = %v", msg["content"])
	}
}

func TestToolResultsBecomeSeparateToolMessagesBeforeProse(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,"messages":[
	  {"role":"user","content":[
	     {"type":"tool_result","tool_use_id":"call_1","content":"first"},
	     {"type":"tool_result","tool_use_id":"call_2","content":[{"type":"text","text":"second"}]},
	     {"type":"text","text":"and a note"}]}]}`)

	messages := parsed["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("want 2 tool messages + 1 user, got %d: %s", len(messages), mustJSON(t, messages))
	}
	if messages[0].(map[string]any)["role"] != "tool" || messages[1].(map[string]any)["role"] != "tool" {
		t.Fatalf("tool results must lead: %s", mustJSON(t, messages))
	}
	if messages[0].(map[string]any)["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v", messages[0])
	}
	if messages[1].(map[string]any)["content"] != "second" {
		t.Errorf("nested block content = %v", messages[1])
	}
	if messages[2].(map[string]any)["content"] != "and a note" {
		t.Errorf("trailing prose = %v", messages[2])
	}
}

func TestToolResultErrorIsPrefixed(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,"messages":[
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"boom","is_error":true}]}]}`)
	msg := parsed["messages"].([]any)[0].(map[string]any)
	if msg["content"] != "Error: boom" {
		t.Errorf("error result = %v", msg["content"])
	}
}

func TestToolsAndToolChoiceMapping(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,
	  "tools":[{"name":"read","description":"read a file","input_schema":{"type":"object"}}],
	  "tool_choice":{"type":"tool","name":"read"},
	  "messages":[{"role":"user","content":"x"}]}`)

	tool := parsed["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v", tool["type"])
	}
	spec := tool["function"].(map[string]any)
	if spec["name"] != "read" || spec["description"] != "read a file" {
		t.Errorf("spec = %v", spec)
	}
	if _, ok := spec["parameters"]; !ok {
		t.Error("input_schema must map to parameters")
	}
	choice, ok := parsed["tool_choice"].(map[string]any)
	if !ok || choice["function"].(map[string]any)["name"] != "read" {
		t.Errorf("named tool_choice = %v", parsed["tool_choice"])
	}
}

func TestToolChoiceAnyMapsToRequired(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,"tool_choice":{"type":"any"},
	  "messages":[{"role":"user","content":"x"}]}`)
	if parsed["tool_choice"] != "required" {
		t.Errorf("any -> %v", parsed["tool_choice"])
	}
}

func TestStreamRequestsAskForUsage(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,"stream":true,
	  "messages":[{"role":"user","content":"x"}]}`)
	opts, ok := parsed["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("stream_options = %v; usage would be unreportable", parsed["stream_options"])
	}
}

func TestMaxTokensDefaultsWhenMissing(t *testing.T) {
	parsed := translate(t, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if got := parsed["max_tokens"].(float64); got != DefaultMaxTokens {
		t.Errorf("max_tokens = %v, want default %d", got, DefaultMaxTokens)
	}
}

func TestStopSequencesBecomeStop(t *testing.T) {
	parsed := translate(t, `{"model":"m","max_tokens":8,"stop_sequences":["a","b"],
	  "messages":[{"role":"user","content":"x"}]}`)
	if got := mustJSON(t, parsed["stop"]); got != `["a","b"]` {
		t.Errorf("stop = %s", got)
	}
}

// ---------- streaming ----------

func chunk(t *testing.T, text string) Chunk {
	t.Helper()
	var c Chunk
	if err := json.Unmarshal([]byte(text), &c); err != nil {
		t.Fatalf("chunk %s: %v", text, err)
	}
	return c
}

func feed(t *testing.T, chunks ...string) []Event {
	t.Helper()
	s := NewStream()
	events := s.Start()
	for _, c := range chunks {
		events = append(events, s.Feed(chunk(t, c))...)
	}
	return append(events, s.Finish()...)
}

// types renders a stream as "type[:kind]" so an expected sequence reads at a glance.
func types(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		label := ev.Type
		if m, ok := ev.Data.(map[string]any); ok {
			if inner, ok := m["delta"].(map[string]any); ok {
				if dt, ok := inner["type"].(string); ok {
					label += "(" + dt + ")"
				}
			}
			if cb, ok := m["content_block"].(map[string]any); ok {
				if bt, ok := cb["type"].(string); ok {
					label += "{" + bt + "}"
				}
			}
		}
		out = append(out, label)
	}
	return out
}

func assertSequence(t *testing.T, events []Event, want string) {
	t.Helper()
	got := strings.Join(types(events), " ")
	if got != want {
		t.Errorf("event sequence:\n got %s\nwant %s", got, want)
	}
}

func TestStreamPlainText(t *testing.T) {
	events := feed(t,
		`{"model":"mimo-v2.5-pro","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`)

	assertSequence(t, events,
		"message_start ping content_block_start{text} content_block_delta(text_delta) "+
			"content_block_delta(text_delta) content_block_stop message_delta message_stop")

	// Both deltas must land on block 0.
	for _, ev := range events {
		m := ev.Data.(map[string]any)
		if m["type"] == "content_block_delta" && m["index"].(int) != 0 {
			t.Errorf("single text stream must stay on index 0, got %v", m["index"])
		}
	}
}

func TestStreamToolUseKeepsArgumentsParsable(t *testing.T) {
	events := feed(t,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)

	var start map[string]any
	var pieces []string
	for _, ev := range events {
		m := ev.Data.(map[string]any)
		switch m["type"] {
		case "content_block_start":
			start = m["content_block"].(map[string]any)
		case "content_block_delta":
			d := m["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				pieces = append(pieces, d["partial_json"].(string))
			}
		}
	}
	if start == nil || start["type"] != "tool_use" {
		t.Fatalf("no tool_use block opened: %v", start)
	}
	// Claude Code rejects a content_block_start that already carries the arguments.
	if input, ok := start["input"].(map[string]any); !ok || len(input) != 0 {
		t.Errorf("tool_use input must start empty, got %v", start["input"])
	}
	if start["id"] != "call_1" || start["name"] != "read" {
		t.Errorf("block header = %v", start)
	}
	joined := strings.Join(pieces, "")
	if joined != `{"path":"a.txt"}` {
		t.Fatalf("reassembled arguments = %s", joined)
	}
	var reassembled map[string]any
	if err := json.Unmarshal([]byte(joined), &reassembled); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if got := types(events); !strings.Contains(strings.Join(got, " "), "message_delta") {
		t.Error("missing terminal message_delta")
	}
}

func TestStreamStopReasonForToolCalls(t *testing.T) {
	events := feed(t,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	last := events[len(events)-2].Data.(map[string]any)
	if got := last["delta"].(map[string]any)["stop_reason"]; got != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", got)
	}
}

func TestStreamTextThenToolSwitchesBlocks(t *testing.T) {
	events := feed(t,
		`{"choices":[{"index":0,"delta":{"content":"thinking out loud"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)

	assertSequence(t, events,
		"message_start ping content_block_start{text} content_block_delta(text_delta) "+
			"content_block_stop content_block_start{tool_use} content_block_delta(input_json_delta) "+
			"content_block_stop message_delta message_stop")

	var indices []int
	for _, ev := range events {
		m := ev.Data.(map[string]any)
		if m["type"] == "content_block_start" {
			indices = append(indices, m["index"].(int))
		}
	}
	if len(indices) != 2 || indices[0] != 0 || indices[1] != 1 {
		t.Errorf("block indices = %v, want [0 1]", indices)
	}
}

func TestStreamThinkingBlock(t *testing.T) {
	events := feed(t,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"yes"},"finish_reason":"stop"}]}`)
	assertSequence(t, events,
		"message_start ping content_block_start{thinking} content_block_delta(thinking_delta) "+
			"content_block_stop content_block_start{text} content_block_delta(text_delta) "+
			"content_block_stop message_delta message_stop")
}

func TestStreamCarriesRealUsage(t *testing.T) {
	events := feed(t,
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45}}`)
	delta := events[len(events)-2].Data.(map[string]any)
	usage := delta["usage"].(map[string]any)
	if usage["input_tokens"] != 123 || usage["output_tokens"] != 45 {
		t.Errorf("usage = %v, want real upstream 123/45", usage)
	}
}

func TestStreamAbortedStillClosesCleanly(t *testing.T) {
	s := NewStream()
	events := s.Start()
	events = append(events, s.Feed(chunk(t, `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`))...)
	events = append(events, s.Finish()...) // upstream died mid-text, no finish_reason

	last := events[len(events)-1].Data.(map[string]any)
	if last["type"] != "message_stop" {
		t.Errorf("stream must still terminate: %v", last)
	}
	if _, ok := events[len(events)-2].Data.(map[string]any)["delta"]; !ok {
		t.Error("expected a message_delta before message_stop")
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
		"function_call": "tool_use", "content_filter": "end_turn", "": "end_turn",
		"brand_new": "end_turn",
	}
	for in, want := range cases {
		if got := MapStopReason(in); got != want {
			t.Errorf("MapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompletionToAnthropicNonStreaming(t *testing.T) {
	raw := `{"id":"cmpl-9","model":"mimo-v2.5-pro",
	  "choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
	    "tool_calls":[{"id":"call_3","type":"function","function":{"name":"read","arguments":"{\"p\":\"x\"}"}}]}}],
	  "usage":{"prompt_tokens":10,"completion_tokens":4}}`
	out, err := CompletionToAnthropic([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	if msg["id"] != "cmpl-9" {
		t.Errorf("id = %v", msg["id"])
	}
	blocks := msg["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("content = %s", mustJSON(t, msg["content"]))
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "read" {
		t.Errorf("block = %v", block)
	}
	if block["input"].(map[string]any)["p"] != "x" {
		t.Errorf("input must be decoded to an object, got %v", block["input"])
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 10 || usage["output_tokens"].(float64) != 4 {
		t.Errorf("usage = %v", usage)
	}
}

func TestOpenAILineParsing(t *testing.T) {
	if _, done, ok := OpenAILine(`data: [DONE]`); !done || ok {
		t.Error("[DONE] must report done")
	}
	if payload, done, ok := OpenAILine(`data: {"a":1}`); done || !ok || payload != `{"a":1}` {
		t.Errorf("data line = %q %v %v", payload, done, ok)
	}
	if _, _, ok := OpenAILine(""); ok {
		t.Error("blank line is not a payload")
	}
	if _, _, ok := OpenAILine(": keepalive"); ok {
		t.Error("comment line is not a payload")
	}
}

func TestWriteEventFrameShape(t *testing.T) {
	var buf strings.Builder
	if err := WriteEvent(&buf, Event{Type: "message_stop", Data: map[string]any{"type": "message_stop"}}); err != nil {
		t.Fatal(err)
	}
	want := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	if buf.String() != want {
		t.Errorf("frame = %q want %q", buf.String(), want)
	}
}
