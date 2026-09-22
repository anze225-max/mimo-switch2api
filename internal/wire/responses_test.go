package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesToChatAcceptsBareStringInput(t *testing.T) {
	out, err := ResponsesToChat([]byte(`{"model":"m","max_output_tokens":32,"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatRequest
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" || chat.Messages[0].Content != "hello" {
		t.Errorf("messages = %+v", chat.Messages)
	}
	if chat.MaxTokens != 32 {
		t.Errorf("max_output_tokens must map to max_tokens, got %d", chat.MaxTokens)
	}
}

func TestResponsesToChatMapsInstructionsAndToolItems(t *testing.T) {
	body := `{"model":"m","instructions":"Be brief","input":[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	  {"type":"function_call","call_id":"call_9","name":"read","arguments":"{\"p\":\"a\"}"},
	  {"type":"function_call_output","call_id":"call_9","output":"file contents"}]}`
	out, err := ResponsesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatRequest
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "Be brief" {
		t.Errorf("instructions -> %+v", chat.Messages[0])
	}
	if chat.Messages[1].Content != "hi" {
		t.Errorf("input_text part lost: %+v", chat.Messages[1])
	}
	call := chat.Messages[2]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != "call_9" {
		t.Errorf("function_call item -> %+v", call)
	}
	if call.ToolCalls[0].Function.Name != "read" || call.ToolCalls[0].Function.Arguments != `{"p":"a"}` {
		t.Errorf("function payload -> %+v", call.ToolCalls[0])
	}
	tool := chat.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "call_9" || tool.Content != "file contents" {
		t.Errorf("function_call_output item -> %+v", tool)
	}
}

func TestChatToResponseKeepsReasoningAndNeverNullsOutput(t *testing.T) {
	out, err := ChatToResponse([]byte(`{"id":"cmpl-1","model":"m",
	  "choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":null,
	    "reasoning_content":"thought about it"}}],
	  "usage":{"prompt_tokens":5,"completion_tokens":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	items, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("output must be an array, got %T (%s)", resp["output"], out)
	}
	if len(items) != 1 {
		t.Fatalf("expected one reasoning item, got %d: %s", len(items), out)
	}
	item := items[0].(map[string]any)
	if item["type"] != "reasoning" {
		t.Errorf("item type = %v", item["type"])
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["text"] != "thought about it" {
		t.Errorf("reasoning text lost: %v", content)
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 12 {
		t.Errorf("total_tokens = %v", usage["total_tokens"])
	}
}

func TestChatToResponseEmptyCompletionStillShapesUp(t *testing.T) {
	out, err := ChatToResponse([]byte(`{"id":"x","model":"m","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	items, ok := resp["output"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("output = %v, want empty array", resp["output"])
	}
}

func feedResponses(t *testing.T, chunks ...string) []Event {
	t.Helper()
	s := NewResponseStream()
	events := s.Start()
	for _, c := range chunks {
		events = append(events, s.Feed(chunk(t, c))...)
	}
	return append(events, s.Finish()...)
}

func TestResponseStreamTextAndReasoning(t *testing.T) {
	events := feedResponses(t,
		`{"model":"mimo-x-pro-preview","choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3}}`)

	var names []string
	for _, ev := range events {
		names = append(names, ev.Type)
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_text.delta",
		"response.output_text.delta", "response.output_text.done", "response.completed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %s", want, joined)
		}
	}
	last := events[len(events)-1].Data.(map[string]any)
	resp := last["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Errorf("status = %v", resp["status"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != 11 || usage["output_tokens"] != 3 {
		t.Errorf("usage = %v, want real 11/3", usage)
	}
	if items, ok := resp["output"].([]map[string]any); !ok || len(items) != 2 {
		t.Errorf("completed response should carry reasoning + message, got %v", resp["output"])
	}
}

// A client that ignores the deltas and reads only response.completed must still find the
// answer there, so the items embedded in it carry content, not just ids.
func TestResponseStreamCompletedCarriesFullItems(t *testing.T) {
	events := feedResponses(t,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"先想想","content":"红色"},"finish_reason":"stop"}]}`)

	raw, err := json.Marshal(events[len(events)-1].Data)
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Response struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	wanted := map[string]string{"message": "红色", "reasoning": "先想想"}
	for item, text := range wanted {
		found := false
		for _, o := range frame.Response.Output {
			if o.Type != item {
				continue
			}
			found = true
			if len(o.Content) != 1 || o.Content[0].Text != text {
				t.Errorf("%s item content = %+v, want %q", item, o.Content, text)
			}
		}
		if !found {
			t.Errorf("no %s item in completed output: %s", item, raw)
		}
	}
}

func TestResponseStreamFunctionCallArgumentsReassemble(t *testing.T) {
	events := feedResponses(t,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]},"finish_reason":"tool_calls"}]}`)

	var args []string
	var doneItem map[string]any
	for _, ev := range events {
		m := ev.Data.(map[string]any)
		if d, ok := m["delta"].(string); ok && ev.Type == "response.function_call_arguments.delta" {
			args = append(args, d)
		}
		if ev.Type == "response.output_item.done" {
			doneItem = m["item"].(map[string]any)
		}
	}
	if joined := strings.Join(args, ""); joined != `{"p":"a"}` {
		t.Errorf("reassembled arguments = %q", joined)
	}
	if doneItem == nil || doneItem["type"] != "function_call" {
		t.Fatalf("no function_call completion item: %v", doneItem)
	}
	if doneItem["call_id"] != "call_1" || doneItem["name"] != "read" {
		t.Errorf("completion item = %v", doneItem)
	}
	if doneItem["arguments"] != `{"p":"a"}` {
		t.Errorf("done item must carry full arguments, got %v", doneItem["arguments"])
	}
}
