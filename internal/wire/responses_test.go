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
	out, err := ChatToResponse(nil, []byte(`{"id":"cmpl-1","model":"m",
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
	out, err := ChatToResponse(nil, []byte(`{"id":"x","model":"m","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
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
	s := NewResponseStream(nil)
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

func TestResponsesToChatAcceptsFlatCodexTools(t *testing.T) {
	// Codex sends the Responses shape: name and parameters sit at the top level of each
	// tool instead of nested under "function" the way Chat Completions does.
	body := `{"model":"m","input":"hi","tool_choice":{"type":"function","name":"exec"},
	  "tools":[{"type":"function","name":"exec","description":"run a command",
	            "parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	out, err := ResponsesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools      []ChatTool `json:"tools"`
		ToolChoice any        `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("flat tool was dropped; chat body = %s", out)
	}
	got := chat.Tools[0]
	if got.Type != "function" || got.Function.Name != "exec" || got.Function.Description != "run a command" {
		t.Errorf("flat tool not wrapped: %+v", got)
	}
	if string(got.Function.Parameters) != `{"type":"object","properties":{"cmd":{"type":"string"}}}` {
		t.Errorf("parameters lost: %s", got.Function.Parameters)
	}
	choice, _ := chat.ToolChoice.(map[string]any)
	fn, _ := choice["function"].(map[string]any)
	if choice["type"] != "function" || fn["name"] != "exec" {
		t.Errorf("flat tool_choice not wrapped: %v", chat.ToolChoice)
	}
}

func TestResponsesToChatSimulatesFreeformAndDropsOnlyHostedTools(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[
	  {"type":"function","function":{"name":"read","parameters":{"type":"object"}}},
	  {"type":"web_search"},
	  {"type":"custom","name":"apply_patch","description":"edit files",
	   "format":{"type":"grammar","syntax":"lark","definition":"start: /./"}}]}`
	plan, chatBody, err := splitPlan(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools []ChatTool `json:"tools"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	// A hosted tool the chat upstream cannot express has to be left out rather than
	// forwarded: one unknown type gets the whole request rejected, costing every tool.
	if len(chat.Tools) != 2 {
		t.Fatalf("tools = %+v, want the function tool plus a simulated freeform one", chat.Tools)
	}
	if chat.Tools[0].Function.Name != "read" {
		t.Errorf("first tool = %+v", chat.Tools[0])
	}
	// The freeform tool must survive as a single-argument function with its grammar folded
	// into the description; dropping it removes Codex's file-editing ability entirely.
	patch := chat.Tools[1]
	if patch.Function.Name != "apply_patch" {
		t.Fatalf("apply_patch was dropped: %+v", chat.Tools)
	}
	if !strings.Contains(patch.Function.Description, "lark") ||
		!strings.Contains(patch.Function.Description, "start: /./") {
		t.Errorf("grammar not carried into description: %q", patch.Function.Description)
	}
	if string(patch.Function.Parameters) != `{"type":"object","properties":{"content":{"type":"string","description":"The tool input."}},"required":["content"]}` {
		t.Errorf("freeform parameters = %s", patch.Function.Parameters)
	}
	if !plan.IsCustom("apply_patch") {
		t.Errorf("plan must remember apply_patch is freeform: %+v", plan)
	}
}

// Codex picks the Responses-Lite layout for every gpt-5.6*/gpt-6* catalog entry: tools
// arrive inside input as a single additional_tools item, wrapped in a "functions"
// namespace, and the top-level tools field is absent. Missing that item discards every
// tool, which is what leaves the model writing calls as prose.
func TestResponsesToChatReadsResponsesLiteAdditionalTools(t *testing.T) {
	body := `{"model":"gpt-6-astra","stream":true,
	  "input":[
	    {"type":"additional_tools","role":"developer","tools":[
	      {"type":"namespace","name":"functions","description":"","tools":[
	        {"type":"function","name":"exec_command","description":"run","strict":false,
	         "parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
	        {"type":"custom","name":"apply_patch","description":"edit",
	         "format":{"type":"grammar","syntax":"lark","definition":"start: /./"}},
	        {"type":"web_search"}]}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}]}`
	plan, chatBody, err := splitPlan(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools []ChatTool `json:"tools"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 2 {
		t.Fatalf("additional_tools was not promoted; tools = %+v (body %s)", chat.Tools, chatBody)
	}
	if chat.Tools[0].Function.Name != "exec_command" {
		t.Errorf("namespaced function tool lost: %+v", chat.Tools[0])
	}
	if string(chat.Tools[0].Function.Parameters) != `{"type":"object","properties":{"cmd":{"type":"string"}}}` {
		t.Errorf("namespaced parameters lost: %s", chat.Tools[0].Function.Parameters)
	}
	// The namespace Codex filed the tool under has to be restored on the way back, or Codex
	// cannot resolve the call.
	if plan.NamespaceFor("exec_command") != "functions" {
		t.Errorf("namespace not remembered: %+v", plan)
	}
	if !plan.IsCustom("apply_patch") {
		t.Errorf("freeform tool lost: %+v", plan)
	}
}

// A reply has to come back in the shape the request used: custom_tool_call with raw
// freeform input for apply_patch, function_call with a namespace for namespaced tools.
func TestResponseStreamRendersCallsInResponsesTerms(t *testing.T) {
	body := `{"model":"gpt-6-astra","stream":true,"input":[{"type":"additional_tools","tools":[
	  {"type":"namespace","name":"functions","tools":[
	    {"type":"function","name":"exec_command","parameters":{"type":"object"}},
	    {"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"d"}}]}]},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`
	plan, _, err := splitPlan(t, body)
	if err != nil {
		t.Fatal(err)
	}
	s := NewResponseStream(plan)
	events := s.Start()
	events = append(events, s.Feed(chunk(t, `{"choices":[{"index":0,"delta":{"tool_calls":[
	  {"index":0,"id":"c1","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}},
	  {"index":1,"id":"c2","function":{"name":"apply_patch","arguments":"{\"content\":\"*** Begin Patch\"}"}}]},
	  "finish_reason":"tool_calls"}]}`))...)
	events = append(events, s.Finish()...)

	byType := map[string]map[string]any{}
	for _, ev := range events {
		if ev.Type != "response.output_item.done" {
			continue
		}
		item := ev.Data.(map[string]any)["item"].(map[string]any)
		byType[item["name"].(string)] = item
	}
	exec, ok := byType["exec_command"]
	if !ok {
		t.Fatalf("no exec_command item: %v", byType)
	}
	if exec["type"] != "function_call" {
		t.Errorf("exec_command type = %v", exec["type"])
	}
	if exec["namespace"] != "functions" {
		t.Errorf("namespace not restored: %v", exec)
	}
	if exec["arguments"] != `{"cmd":"ls"}` || exec["call_id"] != "c1" {
		t.Errorf("exec_command item = %v", exec)
	}
	patch, ok := byType["apply_patch"]
	if !ok {
		t.Fatalf("no apply_patch item: %v", byType)
	}
	if patch["type"] != "custom_tool_call" {
		t.Errorf("apply_patch must come back as custom_tool_call, got %v", patch["type"])
	}
	// The freeform payload has to be un-wrapped back to the raw text Codex's grammar expects.
	if patch["input"] != "*** Begin Patch" {
		t.Errorf("freeform input not restored: %v", patch["input"])
	}
}

// A model that understands the freeform instruction answers with the bare payload rather
// than the JSON wrapper its simulated signature declares — apply_patch in particular returns
// the patch text itself. That raw text is exactly what the grammar asked for, so it must
// reach Codex verbatim instead of being discarded.
func TestToolPlanRenderPassesBareFreeformTextThrough(t *testing.T) {
	plan := &ToolPlan{Custom: map[string]bool{"apply_patch": true}}

	bare := "*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** End Patch"
	if got := plan.Render("call_1", "apply_patch", bare)["input"]; got != bare {
		t.Errorf("bare freeform text mangled:\n got %q\nwant %q", got, bare)
	}

	wrapped := plan.Render("call_1", "apply_patch", `{"content":"*** Begin Patch\n*** End Patch"}`)
	if wrapped["input"] != "*** Begin Patch\n*** End Patch" {
		t.Errorf("wrapped freeform text not unwrapped: %v", wrapped["input"])
	}

	// A literal null content is a real payload ("the tool was called with no text"), not a
	// reason to fall back to the raw JSON.
	if got := plan.Render("call_1", "apply_patch", `{"content":""}`)["input"]; got != "" {
		t.Errorf("empty content should stay empty, got %v", got)
	}
}

// Codex hard-fails a turn whose function_call carries empty arguments.
func TestToolPlanRenderNeverEmitsEmptyArguments(t *testing.T) {
	item := (&ToolPlan{}).Render("call_1", "noop", "")
	if item["arguments"] != "{}" {
		t.Errorf("arguments = %v, want {}", item["arguments"])
	}
}

// Codex drops a reasoning item that has no "summary" field, and hard-fails on usage that is
// missing or non-integral.
func TestCompletedResponseCarriesSummaryAndIntegralUsage(t *testing.T) {
	events := feedResponses(t, `{"choices":[{"index":0,"delta":{"reasoning_content":"t"}}]}`)
	raw, err := json.Marshal(events[len(events)-1].Data)
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Response struct {
			ID     any `json:"id"`
			Output []struct {
				Type    string `json:"type"`
				Summary []any  `json:"summary"`
			} `json:"output"`
			Usage struct {
				Input  *int `json:"input_tokens"`
				Output *int `json:"output_tokens"`
				Total  *int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	if _, ok := frame.Response.ID.(string); !ok {
		t.Errorf("response.id must be a string, got %T", frame.Response.ID)
	}
	for _, item := range frame.Response.Output {
		if item.Type == "reasoning" && item.Summary == nil {
			t.Errorf("reasoning item must carry a summary field")
		}
	}
	if frame.Response.Usage.Input == nil || frame.Response.Usage.Output == nil || frame.Response.Usage.Total == nil {
		t.Errorf("all three usage counts must be present integers: %s", raw)
	}
}

// Codex reads custom_tool_call_input.delta for freeform tools and ignores
// function_call_arguments.delta, so a simulated freeform call must stream under the
// custom event name even though the upstream sends a plain function call.
func TestResponseStreamFreeformUsesCustomDeltaEvent(t *testing.T) {
	plan := &ToolPlan{Custom: map[string]bool{"apply_patch": true}}
	s := NewResponseStream(plan)
	events := s.Start()
	events = append(events, s.Feed(chunk(t, `{"choices":[{"index":0,"delta":{"tool_calls":[
	  {"index":0,"id":"c1","function":{"name":"apply_patch","arguments":"*** Begin"}}]}}]}`))...)
	events = append(events, s.Finish()...)

	sawCustom, sawArgs := false, false
	for _, ev := range events {
		switch ev.Type {
		case "response.custom_tool_call_input.delta":
			sawCustom = true
		case "response.function_call_arguments.delta":
			sawArgs = true
		}
	}
	if !sawCustom {
		t.Errorf("missing response.custom_tool_call_input.delta")
	}
	if sawArgs {
		t.Errorf("freeform call must not emit function_call_arguments.delta")
	}
}

// splitPlan runs the request translation and returns the tool plan alongside the clean chat
// body the upstream would receive.
func splitPlan(t *testing.T, body string) (*ToolPlan, []byte, error) {
	t.Helper()
	chatBody, err := ResponsesToChat([]byte(body))
	if err != nil {
		return nil, nil, err
	}
	plan, clean, err := ToolPlanFromChat(chatBody)
	if err != nil {
		return nil, nil, err
	}
	return plan, clean, nil
}
