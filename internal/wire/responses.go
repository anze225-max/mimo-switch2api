package wire

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// The Responses API (used by Codex) is a third shape on top of Chat Completions:
// history is "input items" instead of messages, and tool calls are function_call /
// function_call_output items rather than role:"tool" turns.
//
// Codex has two wire layouts and which one you get is decided by the model's entry in
// Codex's bundled catalog (models.json inside codex.exe), not by configuration:
//
//   - Classic: tools ride in the request's top-level "tools" field, as
//     {"type":"function","name":…,"parameters":…}.
//   - Responses-Lite: every gpt-5.6*/gpt-6* entry sets `"use_responses_lite": true`, and
//     then (codex-rs/core/src/client.rs) tools move *inside* `input` as a single
//     {"type":"additional_tools","role":"developer","tools":[…]} item, the top-level
//     "tools" field is omitted entirely, and each tool is wrapped by Codex
//     (tools/src/tool_spec.rs, create_tools_json_for_responses_lite) into a
//     {"type":"namespace","name":"functions","tools":[…]} container. File editing arrives
//     as a `custom` tool named apply_patch with a lark grammar, because those catalog
//     entries also set `apply_patch_tool_type: "freeform"`.
//
// Handling only the classic layout silently discards every tool under the lite layout,
// and a model that was never offered a single tool has no choice but to write its
// intended calls out as prose. Both layouts therefore have to be understood.

type ResponsesRequest struct {
	Model           string            `json:"model"`
	Input           json.RawMessage   `json:"input"`
	Instructions    json.RawMessage   `json:"instructions,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	TopP            *float64          `json:"top_p,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	Tools           []json.RawMessage `json:"tools,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
}

// debugTools turns on the per-request tool diagnostics. Set MIMO_DEBUG_TOOLS=1 to trace
// exactly which layout a client used and which tools the model was offered; without it the
// package stays silent so a normal session does not fill the log.
var debugTools = os.Getenv("MIMO_DEBUG_TOOLS") != ""

// toolSet collects the chat tools a Responses request asks for, from every place Codex
// may put them, and remembers the reverse mapping needed to answer in Responses terms.
type toolSet struct {
	chat []ChatTool
	// custom marks a flattened tool name that was simulated from a freeform tool, so a
	// call to it can be re-tagged as custom_tool_call on the way back.
	custom map[string]bool
	// namespace maps a flattened tool name back to the namespace Codex filed it under,
	// because Codex looks the returned call up inside that namespace.
	namespace map[string]string
	// seen guards against the same tool arriving twice (top-level and in additional_tools).
	seen map[string]bool
}

func newToolSet() *toolSet {
	return &toolSet{custom: map[string]bool{}, namespace: map[string]string{}, seen: map[string]bool{}}
}

// addFunction registers one already-normalized function tool.
func (ts *toolSet) addFunction(tool ChatTool, namespace string) {
	if tool.Function.Name == "" || ts.seen[tool.Function.Name] {
		return
	}
	ts.seen[tool.Function.Name] = true
	if namespace != "" {
		ts.namespace[tool.Function.Name] = namespace
	}
	ts.chat = append(ts.chat, tool)
}

// addFreeform simulates a `custom` (freeform) tool as a single-argument function, the way
// LiteLLM does: the chat upstream only understands JSON-schema functions, so the freeform
// payload becomes a `content` string, the original syntax moves into the description, and
// the call is re-tagged as custom on the way back. Dropping these instead is what leaves
// Codex's file-editing tool (apply_patch) missing from the request.
func (ts *toolSet) addFreeform(name, description, format string, namespace string) {
	if name == "" || ts.seen[name] {
		return
	}
	ts.seen[name] = true
	ts.custom[name] = true
	if namespace != "" {
		ts.namespace[name] = namespace
	}
	if format != "" {
		if description != "" {
			description += "\n\n"
		}
		description += "The input must be a single string that conforms to this grammar:\n" + format
	}
	ts.chat = append(ts.chat, ChatTool{Type: "function", Function: ChatToolSpec{
		Name:        name,
		Description: description,
		Parameters: json.RawMessage(`{"type":"object","properties":{"content":` +
			`{"type":"string","description":"The tool input."}},"required":["content"]}`),
	}})
}

// addTool dispatches one element of a Responses tools array in either shape.
func (ts *toolSet) addTool(raw json.RawMessage, namespace string) {
	var tool struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Function    *ChatToolSpec   `json:"function"`
		// namespace containers nest their own tools one level down.
		Tools []json.RawMessage `json:"tools"`
		// custom/freeform tools describe their payload with a grammar.
		Format *struct {
			Type       string `json:"type"`
			Syntax     string `json:"syntax"`
			Definition string `json:"definition"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		log.Printf("responses: ignoring unparsable tool: %v", err)
		return
	}

	switch firstNonEmpty(tool.Type, "function") {
	case "function":
		switch {
		case tool.Function != nil && tool.Function.Name != "":
			ts.addFunction(ChatTool{Type: "function", Function: *tool.Function}, namespace)
		case tool.Name != "":
			ts.addFunction(ChatTool{Type: "function", Function: ChatToolSpec{
				Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters,
			}}, namespace)
		}
	case "namespace":
		// Codex nests real tools inside "functions"; recurse so they stay reachable.
		inner := firstNonEmpty(tool.Name, "functions")
		for _, nested := range tool.Tools {
			ts.addTool(nested, inner)
		}
	case "custom":
		definition := ""
		if tool.Format != nil {
			definition = strings.TrimSpace(strings.Join(
				[]string{tool.Format.Syntax, tool.Format.Definition}, " "))
		}
		ts.addFreeform(tool.Name, tool.Description, definition, namespace)
	default:
		// Hosted tools (web_search, tool_search, local_shell, image_generation) have no
		// chat-completions equivalent, and forwarding one unknown type makes the upstream
		// reject the whole request — which would cost the client every tool it sent.
		if debugTools {
			log.Printf("responses: dropping hosted tool %q (type %q): not expressible as a chat function",
				tool.Name, tool.Type)
		}
	}
}

// chatToolChoice maps the flat Responses function choice ({"type":"function","name":…}) onto
// the wrapped Chat form; the string modes ("auto", "required", "none") read the same in both.
func chatToolChoice(choice any) any {
	top, ok := choice.(map[string]any)
	if !ok || top["type"] != "function" {
		return choice
	}
	name, ok := top["name"].(string)
	if !ok || name == "" {
		return choice // already wrapped, or nothing to map
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}
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
		ToolChoice:  chatToolChoice(req.ToolChoice),
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

	tools := newToolSet()
	for _, raw := range req.Tools {
		tools.addTool(raw, "")
	}
	var liteItems int

	for _, item := range items {
		switch item.Type {
		case "additional_tools":
			// Responses-Lite carries the entire tool list here, one item per request.
			liteItems++
			for _, raw := range item.Tools {
				tools.addTool(raw, "")
			}
		case "function_call":
			// Calls made in an earlier turn are history; keep them in the transcript so the
			// model can see what it already did. Consecutive calls belong to one assistant
			// turn and must be merged, because strict upstreams reject back-to-back
			// assistant messages.
			appendToolCall(&out.Messages, ChatToolCall{
				ID:       firstNonEmpty(item.CallID, item.ID),
				Type:     "function",
				Function: ChatFunction{Name: item.Name, Arguments: item.Arguments},
			})
		case "custom_tool_call":
			// A previous turn's freeform call. Present it as the function call it was
			// simulated as, so the history matches the tool list we send.
			appendToolCall(&out.Messages, ChatToolCall{
				ID:   firstNonEmpty(item.CallID, item.ID),
				Type: "function",
				Function: ChatFunction{
					Name:      item.Name,
					Arguments: freeformArguments(item.Input),
				},
			})
		case "function_call_output", "custom_tool_call_output":
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
	out.Tools = tools.chat
	if len(out.Tools) == 0 {
		// An empty tools array alongside a tool_choice is itself a rejected request; when
		// there is nothing to choose from, say nothing at all.
		out.ToolChoice = nil
	}
	// One line per request so a real client session can be inspected without a packet
	// capture: which layout arrived, and what the model was actually offered. Off by
	// default because the tool list of every request is noisy in normal use.
	if debugTools {
		if liteItems > 0 || len(tools.chat) > 0 {
			names := make([]string, 0, len(tools.chat))
			for _, t := range tools.chat {
				mark := ""
				if tools.custom[t.Function.Name] {
					mark = "(freeform)"
				}
				names = append(names, t.Function.Name+mark)
			}
			log.Printf("responses: model=%s lite_tool_items=%d top_level_tools=%d -> offering %d tools: %s",
				req.Model, liteItems, len(req.Tools), len(tools.chat), strings.Join(names, ", "))
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return encodeToolPlan(encoded, tools)
}

// appendToolCall adds a tool call to the last assistant turn when it is still open, since
// one assistant turn may contain several calls.
func appendToolCall(messages *[]ChatMessage, call ChatToolCall) {
	msgs := *messages
	if n := len(msgs); n > 0 {
		last := &msgs[n-1]
		if last.Role == "assistant" && last.ToolCallID == "" && len(last.ToolCalls) > 0 {
			last.ToolCalls = append(last.ToolCalls, call)
			return
		}
	}
	*messages = append(msgs, ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{call}})
}

// freeformArguments wraps a freeform tool's raw input into the single-argument JSON object
// its simulated function signature declares.
func freeformArguments(input string) string {
	if strings.TrimSpace(input) == "" {
		return "{}"
	}
	wrapped, err := json.Marshal(map[string]any{"content": input})
	if err != nil {
		return "{}"
	}
	return string(wrapped)
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
	// Responses-Lite puts the whole tool list in an additional_tools item.
	Tools []json.RawMessage `json:"tools"`
	// Freeform (custom) calls carry their payload as "input", not "arguments".
	Input string `json:"input"`
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

// ---------- tool plan ----------
//
// The reply has to be re-tagged in Responses terms, but the chat completion that produces
// it carries only the name we sent (a flattened, namespace-less function name). The plan
// therefore tags the outgoing chat body so the reply can be matched back to it.

const toolPlanField = "_mimo_tool_plan"

// ToolPlan carries the reverse mapping from a flattened chat tool name back to the
// Responses shape Codex expects.
type ToolPlan struct {
	Custom    map[string]bool   `json:"custom,omitempty"`
	Namespace map[string]string `json:"namespace,omitempty"`
}

// encodeToolPlan attaches the reverse mapping alongside the chat body. The plan stays
// inside this process: the server reads it back out before posting upstream.
func encodeToolPlan(chatBody []byte, ts *toolSet) ([]byte, error) {
	if len(ts.custom) == 0 && len(ts.namespace) == 0 {
		return chatBody, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(chatBody, &doc); err != nil {
		return nil, err
	}
	plan, err := json.Marshal(ToolPlan{Custom: ts.custom, Namespace: ts.namespace})
	if err != nil {
		return nil, err
	}
	doc[toolPlanField] = plan
	return json.Marshal(doc)
}

// ToolPlanFromChat extracts the reverse mapping from a chat body produced by
// ResponsesToChat, returning the body with the private field removed so it can be posted
// upstream untouched. A body without a plan returns (nil, chatBody, nil).
func ToolPlanFromChat(chatBody []byte) (*ToolPlan, []byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(chatBody, &doc); err != nil {
		return nil, chatBody, err
	}
	raw, ok := doc[toolPlanField]
	if !ok {
		return nil, chatBody, nil
	}
	delete(doc, toolPlanField)
	clean, err := json.Marshal(doc)
	if err != nil {
		return nil, chatBody, err
	}
	var plan ToolPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, clean, nil
	}
	return &plan, clean, nil
}

// IsCustom reports whether the named tool was simulated from a freeform/custom tool.
func (p *ToolPlan) IsCustom(name string) bool {
	return p != nil && p.Custom[name]
}

// NamespaceFor returns the namespace Codex filed the tool under, if any.
func (p *ToolPlan) NamespaceFor(name string) string {
	if p == nil {
		return ""
	}
	return p.Namespace[name]
}

// Render converts one chat tool call into the Responses item Codex expects: a
// custom_tool_call carrying the raw freeform text for simulated freeform tools, and a
// function_call otherwise, with the namespace restored when the tool came from one.
func (p *ToolPlan) Render(callID, name, arguments string) map[string]any {
	if p.IsCustom(name) {
		return map[string]any{
			"type": "custom_tool_call", "id": newID("ctc"), "call_id": callID,
			"name": name, "input": freeformInput(arguments), "status": "completed",
		}
	}
	if arguments == "" {
		// Codex fails the turn with "failed to parse function arguments" on an empty
		// string; an empty object is the correct no-argument payload.
		arguments = "{}"
	}
	item := map[string]any{
		"type": "function_call", "id": newID("fc"), "call_id": callID,
		"name": name, "arguments": arguments, "status": "completed",
	}
	if ns := p.NamespaceFor(name); ns != "" {
		item["namespace"] = ns
	}
	return item
}

// freeformInput recovers the raw text a simulated freeform tool was called with.
//
// The simulated signature declares a single `content` string, but a model that understands
// the freeform instruction tends to answer with the bare text the grammar describes rather
// than the JSON wrapper — apply_patch in particular returns the patch itself. Both shapes
// therefore have to be accepted, and anything that is not the expected wrapper is passed
// through verbatim, because it is the payload the freeform grammar asked for.
func freeformInput(arguments string) string {
	var decoded struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal([]byte(arguments), &decoded); err == nil && decoded.Content != nil {
		return *decoded.Content
	}
	return arguments
}

// atLeastZero keeps usage counters integral; Codex hard-fails on a missing or fractional
// token count.
func atLeastZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// ---------- non-streaming rendering ----------

// ChatToResponse renders a chat completion as a Responses output object. The plan is the
// one produced when the matching request was translated; nil renders plain function_calls.
func ChatToResponse(plan *ToolPlan, chatBody []byte) ([]byte, error) {
	var c Completion
	if err := json.Unmarshal(chatBody, &c); err != nil {
		return nil, err
	}
	// output must always be an array: clients treat null as a malformed response.
	items := []map[string]any{}
	text := ""
	status := "completed"
	incomplete := any(nil)
	if len(c.Choices) > 0 {
		msg := c.Choices[0].Message
		if think := msg.ReasoningContent.String(); think != "" {
			items = append(items, reasoningItem(newID("rs"), think))
		}
		if body := msg.Content.String(); body != "" {
			text = body
			items = append(items, messageItem(newID("msg"), body))
		}
		for _, tc := range msg.ToolCalls {
			items = append(items, plan.Render(toolCallID(tc.ID), tc.Function.Name, tc.Function.Arguments))
		}
		if c.Choices[0].FinishReason == "length" {
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		}
	}
	id := c.ID
	if id == "" {
		id = newID("resp")
	}
	return json.Marshal(map[string]any{
		"id": id, "object": "response", "created_at": 0, "status": status,
		"model": c.Model, "output": items, "output_text": text,
		"error": nil, "incomplete_details": incomplete,
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		"temperature": nil, "top_p": nil, "truncation": "disabled", "user": nil,
		"metadata": map[string]any{},
		"usage": map[string]any{
			"input_tokens":  atLeastZero(c.Usage.PromptTokens),
			"output_tokens": atLeastZero(c.Usage.CompletionTokens),
			"total_tokens":  atLeastZero(c.Usage.PromptTokens) + atLeastZero(c.Usage.CompletionTokens),
		},
	})
}

// toolCallID falls back to a generated id so a call is never emitted without one.
func toolCallID(id string) string {
	if id != "" {
		return id
	}
	return newID("call")
}

// reasoningItem builds a reasoning output item. Codex silently drops any reasoning item
// that has no "summary" field, so it is always present.
func reasoningItem(id, text string) map[string]any {
	return map[string]any{
		"type": "reasoning", "id": id, "role": "assistant", "status": "completed",
		"summary": []any{},
		"content": []map[string]any{{"type": "reasoning_text", "text": text}},
	}
}

func messageItem(id, text string) map[string]any {
	return map[string]any{
		"type": "message", "id": id, "role": "assistant", "status": "completed",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

// ---------- streaming rendering ----------

// ResponseStream turns a chat SSE stream into Responses SSE events. The plan, when set,
// decides whether each call is rendered as a function_call or a custom_tool_call.
type ResponseStream struct {
	plan       *ToolPlan
	responseID string
	model      string
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
	incomplete bool
}

type respTool struct {
	itemID, callID, name string
	index                int
	args                 strings.Builder
}

func NewResponseStream(plan *ToolPlan) *ResponseStream {
	return &ResponseStream{plan: plan, responseID: newID("resp"), tools: map[int]*respTool{}}
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
	if chunk.Model != "" {
		s.model = chunk.Model
	}
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
				"item": map[string]any{"type": "reasoning", "id": s.reasoningItemID, "role": "assistant", "summary": []any{}},
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
				callID: toolCallID(tc.ID),
				name:   tc.Function.Name,
				index:  len(s.items),
			}
			s.tools[index] = block
			s.toolOrder = append(s.toolOrder, index)
			// The placeholder is replaced in Finish, once the arguments are complete and
			// the tool is known to be a function_call or a custom_tool_call.
			s.items = append(s.items, map[string]any{"type": "function_call", "id": block.itemID})
			events = append(events, Event{Type: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "output_index": block.index,
				"item": s.plan.addedItem(block),
			}})
		}
		if block.name == "" && tc.Function.Name != "" {
			block.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			block.args.WriteString(tc.Function.Arguments)
			// Codex parses custom_tool_call_input.delta for freeform tools and ignores
			// function_call_arguments.delta, so the event name has to follow the item kind.
			if s.plan.IsCustom(block.name) {
				events = append(events, Event{Type: "response.custom_tool_call_input.delta", Data: map[string]any{
					"type": "response.custom_tool_call_input.delta", "item_id": block.itemID,
					"output_index": block.index, "delta": tc.Function.Arguments,
				}})
			} else {
				events = append(events, Event{Type: "response.function_call_arguments.delta", Data: map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": block.itemID,
					"output_index": block.index, "delta": tc.Function.Arguments,
				}})
			}
		}
	}
	if reason := chunk.Choices[0].FinishReason; reason == "length" {
		s.incomplete = true
	}
	return events
}

// addedItem renders the opening item for a tool block: a custom_tool_call announces itself
// with "input" while a function_call announces itself with "arguments".
func (p *ToolPlan) addedItem(block *respTool) map[string]any {
	if p.IsCustom(block.name) {
		return map[string]any{
			"type": "custom_tool_call", "id": block.itemID, "call_id": block.callID,
			"name": block.name, "input": "",
		}
	}
	item := map[string]any{
		"type": "function_call", "id": block.itemID, "call_id": block.callID, "name": block.name,
	}
	if ns := p.NamespaceFor(block.name); ns != "" {
		item["namespace"] = ns
	}
	return item
}

// Usage reports the token counts the upstream itself returned for this stream.
func (s *ResponseStream) Usage() (promptTokens, completionTokens int) {
	return s.inputTok, s.outputTok
}

func (s *ResponseStream) Finish() []Event {
	var events []Event
	if s.reasoningItemID != "" {
		item := reasoningItem(s.reasoningItemID, s.reasoning.String())
		// The completed payload must carry what the deltas delivered, so publish the same item.
		s.items[s.reasoningIndex] = item
		events = append(events, Event{Type: "response.output_item.done", Data: map[string]any{
			"type": "response.output_item.done", "output_index": s.reasoningIndex, "item": item,
		}})
	}
	if s.textItemID != "" {
		item := messageItem(s.textItemID, s.text.String())
		s.items[s.textIndex] = item
		events = append(events, Event{Type: "response.output_text.done", Data: map[string]any{
			"type": "response.output_text.done", "item_id": s.textItemID, "text": s.text.String(),
		}}, Event{Type: "response.output_item.done", Data: map[string]any{
			"type": "response.output_item.done", "output_index": s.textIndex, "item": item,
		}})
	}
	for _, key := range s.toolOrder {
		block := s.tools[key]
		item := s.plan.Render(block.callID, block.name, block.args.String())
		item["id"] = block.itemID
		s.items[block.index] = item
		events = append(events, Event{Type: "response.output_item.done", Data: map[string]any{
			"type": "response.output_item.done", "output_index": block.index, "item": item,
		}})
	}
	status, incomplete := "completed", any(nil)
	if s.incomplete {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	events = append(events, Event{Type: "response.completed", Data: map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": s.responseID, "object": "response", "status": status,
			"model": s.model, "output": s.items,
			"error": nil, "incomplete_details": incomplete,
			"usage": map[string]any{
				"input_tokens":  atLeastZero(s.inputTok),
				"output_tokens": atLeastZero(s.outputTok),
				"total_tokens":  atLeastZero(s.inputTok) + atLeastZero(s.outputTok),
			},
		},
	}})
	return events
}

var _ = strconv.Itoa
