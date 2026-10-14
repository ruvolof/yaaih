package llm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// doneStream is the shortest complete response a provider can send: one content
// delta, a usage block, and the terminator.
func doneStream(w http.ResponseWriter, flush func(), _ func()) {
	fmt.Fprint(w, chunkEvent("stub", "ok"))
	fmt.Fprint(w, usageEvent)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

// toolCallChunk is one streamed slice of a tool call. Providers split the
// arguments across chunks, and only the first carries the id and the name.
func toolCallChunk(delta string) string {
	return fmt.Sprintf("data: {\"id\":\"stub\",\"object\":\"chat.completion.chunk\",\"model\":\"stub\","+
		"\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[%s]}}]}\n\n", delta)
}

func mapAt(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%q = %#v, want an object", key, parent[key])
	}
	return value
}

func sliceAt(t *testing.T, parent map[string]any, key string) []any {
	t.Helper()
	value, ok := parent[key].([]any)
	if !ok {
		t.Fatalf("%q = %#v, want an array", key, parent[key])
	}
	return value
}

func elem(t *testing.T, items []any, i int) map[string]any {
	t.Helper()
	if i >= len(items) {
		t.Fatalf("element %d requested from a %d-element array", i, len(items))
	}
	value, ok := items[i].(map[string]any)
	if !ok {
		t.Fatalf("element %d = %#v, want an object", i, items[i])
	}
	return value
}

func wantField(t *testing.T, where string, obj map[string]any, key string, want any) {
	t.Helper()
	if got := obj[key]; got != want {
		t.Errorf("%s.%s = %#v, want %#v", where, key, got, want)
	}
}

// A tool call and its result are a pair: the provider rejects an assistant
// message whose tool_calls have no matching tool reply, and it matches them by
// id. This pins the shape of both halves as they leave the harness.
func TestToolCallRoundTripReachesTheWireIntact(t *testing.T) {
	srv, _, requests := sseServer(t, doneStream)
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	req := &ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "you are a harness"},
			{Role: RoleUser, Content: "list the workspace"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"ls"}`},
			}},
			{Role: RoleTool, ToolCallID: "call-1", Content: "alpha\nbeta"},
		},
	}
	if _, err := provider.StreamChat(context.Background(), req, nil); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	payload := requests.decode(t, 0)
	wantField(t, "request", payload, "model", "stub-model")
	if _, present := payload["tools"]; present {
		t.Error("tools was sent for a request that granted none")
	}

	messages := sliceAt(t, payload, "messages")
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(messages))
	}
	wantField(t, "messages[0]", elem(t, messages, 0), "role", "system")
	wantField(t, "messages[0]", elem(t, messages, 0), "content", "you are a harness")
	wantField(t, "messages[1]", elem(t, messages, 1), "role", "user")
	wantField(t, "messages[1]", elem(t, messages, 1), "content", "list the workspace")

	assistant := elem(t, messages, 2)
	wantField(t, "messages[2]", assistant, "role", "assistant")
	calls := sliceAt(t, assistant, "tool_calls")
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	call := elem(t, calls, 0)
	wantField(t, "tool_calls[0]", call, "id", "call-1")
	wantField(t, "tool_calls[0]", call, "type", "function")
	fn := mapAt(t, call, "function")
	wantField(t, "tool_calls[0].function", fn, "name", "shell_exec")
	wantField(t, "tool_calls[0].function", fn, "arguments", `{"command":"ls"}`)

	reply := elem(t, messages, 3)
	wantField(t, "messages[3]", reply, "role", "tool")
	wantField(t, "messages[3]", reply, "tool_call_id", "call-1")
	wantField(t, "messages[3]", reply, "content", "alpha\nbeta")
}

func TestToolDefinitionsReachTheWire(t *testing.T) {
	srv, _, requests := sseServer(t, doneStream)
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
		},
		"required": []string{"command"},
	}
	req := &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
		Tools:    []ToolDefinition{{Name: "shell_exec", Description: "run a command", Parameters: schema}},
	}
	if _, err := provider.StreamChat(context.Background(), req, nil); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	tools := sliceAt(t, requests.decode(t, 0), "tools")
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	tool := elem(t, tools, 0)
	wantField(t, "tools[0]", tool, "type", "function")
	fn := mapAt(t, tool, "function")
	wantField(t, "tools[0].function", fn, "name", "shell_exec")
	wantField(t, "tools[0].function", fn, "description", "run a command")

	params := mapAt(t, fn, "parameters")
	wantField(t, "tools[0].function.parameters", params, "type", "object")
	properties := mapAt(t, params, "properties")
	if _, ok := properties["command"]; !ok {
		t.Errorf("parameters.properties = %#v, want the declared command property", properties)
	}
}

// Providers stream a tool call in pieces: the first chunk carries the id and
// the name, later ones only extend the arguments. Handing the model's arguments
// to json.Unmarshal a piece at a time would fail, so they have to arrive
// reassembled.
func TestStreamChatReassemblesSplitToolCalls(t *testing.T) {
	srv, _, _ := sseServer(t, func(w http.ResponseWriter, flush func(), _ func()) {
		fmt.Fprint(w, toolCallChunk(`{"index":0,"id":"call-1","type":"function","function":{"name":"shell_exec","arguments":"{\"comm"}}`))
		flush()
		fmt.Fprint(w, toolCallChunk(`{"index":0,"function":{"arguments":"and\":\"ls -la\"}"}}`))
		flush()
		fmt.Fprint(w, usageEvent)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	})
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	var deltas []string
	resp, err := provider.StreamChat(context.Background(), stubRequest(), func(ev StreamEvent) {
		if ev.DeltaText != "" {
			deltas = append(deltas, ev.DeltaText)
		}
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want the two chunks merged into one", resp.ToolCalls)
	}
	call := resp.ToolCalls[0]
	if call.ID != "call-1" || call.Name != "shell_exec" {
		t.Errorf("tool call = %+v, want call-1/shell_exec", call)
	}
	if call.Arguments != `{"command":"ls -la"}` {
		t.Errorf("arguments = %q, want the reassembled JSON", call.Arguments)
	}
	// Tool arguments are not prose; streaming them to the chat view would show
	// the operator raw JSON fragments mid-turn.
	if len(deltas) != 0 {
		t.Errorf("content deltas = %v, want tool arguments kept off the stream callback", deltas)
	}
}

// Cost is the only budget signal the harness gets, and it rides in the same
// usage block as the token counts, as a field only OpenRouter sends.
func TestOpenRouterCostIsReadFromTheUsageChunk(t *testing.T) {
	const usageWithCost = `data: {"id":"stub","object":"chat.completion.chunk","model":"stub",` +
		`"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,` +
		`"cost":0.00042,"cost_details":{"upstream_inference_cost":0.0004}}}` + "\n\n"

	srv, _, _ := sseServer(t, func(w http.ResponseWriter, flush func(), _ func()) {
		fmt.Fprint(w, chunkEvent("stub", "hello"))
		fmt.Fprint(w, usageWithCost)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	})

	// Cost parsing is switched on by the base URL naming OpenRouter, which is
	// all the provider has to go on; a path suffix is enough to reach the same
	// branch against a local server.
	provider := NewOpenAICompat(srv.URL+"/openrouter.ai", "test-key", "stub-model")
	if !strings.Contains(provider.Name(), "openrouter.ai") {
		t.Fatalf("Name() = %q, want the base URL it was configured with", provider.Name())
	}

	resp, err := provider.StreamChat(context.Background(), stubRequest(), nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if !resp.Usage.CostAvailable {
		t.Fatal("cost was reported as unavailable although the chunk carried it")
	}
	if resp.Usage.Cost != 0.00042 {
		t.Errorf("cost = %v, want 0.00042", resp.Usage.Cost)
	}
}

// A provider that reports no cost has to leave the sidebar showing "N/A"
// rather than a confident zero.
func TestMissingCostIsNotReportedAsZero(t *testing.T) {
	srv, _, _ := sseServer(t, doneStream)
	provider := NewOpenAICompat(srv.URL+"/openrouter.ai", "test-key", "stub-model")

	resp, err := provider.StreamChat(context.Background(), stubRequest(), nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if resp.Usage.CostAvailable {
		t.Errorf("cost reported as available (%v) for a usage block without one", resp.Usage.Cost)
	}
}
