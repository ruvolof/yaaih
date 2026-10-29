package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/executor"
	"github.com/ruvolof/yaaih/internal/governor"
	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/session"
)

// stubProvider replays scripted responses in order and keeps the requests it
// was given, so a test can assert on what the model was actually offered.
type stubProvider struct {
	mu        sync.Mutex
	responses []*llm.ChatResponse
	requests  []*llm.ChatRequest
}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) StreamChat(ctx context.Context, req *llm.ChatRequest, cb llm.StreamCallback) (*llm.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return nil, errors.New("stub provider: ran out of scripted responses")
	}
	resp := p.responses[0]
	p.responses = p.responses[1:]
	if cb != nil && resp.Content != "" {
		cb(llm.StreamEvent{DeltaText: resp.Content})
	}
	return resp, nil
}

func (p *stubProvider) request(i int) *llm.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.requests) {
		return nil
	}
	return p.requests[i]
}

// textResponse is a plain end-of-turn reply. The non-zero usage keeps it clear
// of the empty-glitch retry, which keys on a response with no usage at all.
func textResponse(content string) *llm.ChatResponse {
	return &llm.ChatResponse{
		Content: content,
		Usage:   llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}
}

func toolCallResponse(id, name, args string) *llm.ChatResponse {
	return &llm.ChatResponse{
		ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: args}},
		Usage:     llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}
}

// stubRunner stands in for the sandbox executor and counts what reached it.
// output, when set, is what every command returns; every command finishes at
// once.
type stubRunner struct {
	mu       sync.Mutex
	commands []string
	output   string
}

func (r *stubRunner) Start(ctx context.Context, cmd string, wait time.Duration) (executor.JobResult, error) {
	return r.Run(ctx, cmd)
}

func (r *stubRunner) Run(ctx context.Context, cmd string) (executor.JobResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, cmd)
	output := r.output
	if output == "" {
		output = "stub output"
	}
	return executor.JobResult{ID: len(r.commands), Output: output, StateKept: true}, nil
}

func (r *stubRunner) Wait(ctx context.Context, id int, wait time.Duration) (executor.JobResult, error) {
	return executor.JobResult{ID: id}, nil
}

func (r *stubRunner) Kill(ctx context.Context, id int) (executor.JobResult, error) {
	return executor.JobResult{ID: id, ExitCode: -1}, nil
}

func (r *stubRunner) Jobs() []executor.JobInfo         { return nil }
func (r *stubRunner) TakeFinished() []executor.JobInfo { return nil }
func (r *stubRunner) CWD() string                      { return "/workspace" }
func (r *stubRunner) IsRunning() bool                  { return true }
func (r *stubRunner) Close() error                     { return nil }

func (r *stubRunner) executed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

func toolDef(name string) llm.ToolDefinition {
	return llm.ToolDefinition{Name: name, Description: "test tool", Parameters: emptyObjectSchema()}
}

func newTestAgent(t *testing.T, caps []string, prov llm.Provider, runner executor.Executor) *Agent {
	t.Helper()
	return newTestAgentWithOptions(t, caps, prov, runner, config.Options{
		MaxToolIterations:  5,
		MaxToolResultBytes: testToolResultBudget,
		MaxFileReadLines:   50,
	})
}

// testToolResultBudget is small enough that a test can overrun it with a
// readable amount of text.
const testToolResultBudget = 2048

func newTestAgentWithOptions(t *testing.T, caps []string, prov llm.Provider, runner executor.Executor, opts config.Options) *Agent {
	t.Helper()
	cfg := &config.Config{
		Model:        "stub-model",
		Capabilities: caps,
		Runtime:      config.RuntimeCfg{Workspace: t.TempDir()},
		Options:      opts,
	}
	return New(cfg, prov, governor.New(cfg.Model), governor.New(cfg.Model),
		runner, nil, NewGatekeeper(false), nil, session.Transcript{}, t.TempDir())
}

// runTurn drives one turn to completion and returns everything the agent
// emitted. The agent is closed afterwards, which is what ends the collector.
func runTurn(t *testing.T, ag *Agent, input string) []Event {
	t.Helper()
	return runTurnCtx(t, ag, context.Background(), input)
}

// runTurns drives several turns against one agent, the way a session that grows
// over its context budget accumulates them.
func runTurns(t *testing.T, ag *Agent, inputs ...string) []Event {
	t.Helper()
	return runTurnCtx(t, ag, context.Background(), inputs...)
}

// runTurnCtx is runTurn with a caller-supplied context, so a test can cancel
// the turn part-way through the way the operator's stop key does.
func runTurnCtx(t *testing.T, ag *Agent, ctx context.Context, inputs ...string) []Event {
	t.Helper()
	return collectEvents(t, ag, func() {
		for _, input := range inputs {
			ag.Send(ctx, input)
		}
	})
}

// collectEvents runs actions with a reader draining the agent's event channel —
// which only holds 32 events before the agent blocks on it — and returns
// everything they emitted. The agent is closed afterwards, which is what ends
// the collector.
func collectEvents(t *testing.T, ag *Agent, actions func()) []Event {
	t.Helper()
	collected := make(chan []Event, 1)
	go func() {
		var events []Event
		for ev := range ag.Events() {
			events = append(events, ev)
		}
		collected <- events
	}()

	actions()
	ag.Close()

	select {
	case events := <-collected:
		return events
	case <-time.After(10 * time.Second):
		t.Fatal("timed out collecting agent events")
		return nil
	}
}

func toolResults(events []Event) []ToolCallResultMsg {
	var out []ToolCallResultMsg
	for _, ev := range events {
		if msg, ok := ev.(ToolCallResultMsg); ok {
			out = append(out, msg)
		}
	}
	return out
}

func errorMessages(events []Event) []string {
	var out []string
	for _, ev := range events {
		if msg, ok := ev.(ErrorMsg); ok {
			out = append(out, msg.Err)
		}
	}
	return out
}

func offersTool(req *llm.ChatRequest, name string) bool {
	if req == nil {
		return false
	}
	for _, def := range req.Tools {
		if def.Name == name {
			return true
		}
	}
	return false
}

// TestSendRefusesToolOutsideCapabilities is A1 end to end: the model calls a
// tool it was never offered — trivially reachable by resuming a devops
// transcript under an agent without the shell capability — and the turn must
// answer it with an error the model can recover from, never reaching the tool.
func TestSendRefusesToolOutsideCapabilities(t *testing.T) {
	runner := &stubRunner{}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"rm -rf /"}`),
		textResponse("understood, I have no shell here"),
	}}
	agent := newTestAgent(t, []string{"filesystem"}, provider, runner)

	events := runTurn(t, agent, "clean up the disk")

	if commands := runner.executed(); len(commands) != 0 {
		t.Errorf("executor ran %v for a tool the agent was never granted", commands)
	}
	if offersTool(provider.request(0), "shell_exec") {
		t.Error("shell_exec was offered to a model without the shell capability")
	}

	results := toolResults(events)
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
	if !strings.Contains(results[0].Err, "unknown tool") {
		t.Errorf("tool result err = %q, want it to name the tool as unknown", results[0].Err)
	}

	// The rejection has to reach the model as a tool reply, or the next
	// request carries an unanswered tool_call and the provider rejects it.
	second := provider.request(1)
	if second == nil {
		t.Fatal("the turn ended without giving the model a chance to self-correct")
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != llm.RoleTool || last.ToolCallID != "call-1" {
		t.Fatalf("last message = %+v, want a tool reply to call-1", last)
	}
	if !strings.Contains(last.Content, "unknown tool") {
		t.Errorf("tool reply content = %q, want it to name the tool as unknown", last.Content)
	}
	if results[0].Result != last.Content {
		t.Errorf("operator was shown %q, but the model was given %q", results[0].Result, last.Content)
	}

	if errs := errorMessages(events); len(errs) != 0 {
		t.Errorf("turn reported errors %v, want the model to recover on its own", errs)
	}
}

// TestSendRunsToolWithinCapabilities is the positive control for the test
// above: the same call against an agent that declares the capability reaches
// the executor.
func TestSendRunsToolWithinCapabilities(t *testing.T) {
	runner := &stubRunner{}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"ls -la"}`),
		textResponse("here is the listing"),
	}}
	agent := newTestAgent(t, []string{"shell"}, provider, runner)

	events := runTurn(t, agent, "list the workspace")

	if commands := runner.executed(); len(commands) != 1 || commands[0] != "ls -la" {
		t.Fatalf("executor ran %v, want [ls -la]", commands)
	}
	if !offersTool(provider.request(0), "shell_exec") {
		t.Error("shell_exec was not offered to an agent granted the shell capability")
	}
	results := toolResults(events)
	if len(results) != 1 || results[0].Err != "" || results[0].Result != "stub output" {
		t.Fatalf("tool results = %+v, want one successful result", results)
	}
}

// TestSendRefusesShellWithoutSandbox covers an agent that declares a
// capability whose sandbox never came up. The tool has no executor to close
// over, so calling it has to be refused rather than dispatched into nothing.
func TestSendRefusesShellWithoutSandbox(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"ls"}`),
		textResponse("no sandbox here"),
	}}
	agent := newTestAgent(t, []string{"shell"}, provider, nil)

	events := runTurn(t, agent, "list the workspace")

	results := toolResults(events)
	if len(results) != 1 || !strings.Contains(results[0].Err, "unknown tool") {
		t.Fatalf("tool results = %+v, want one unknown-tool rejection", results)
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Errorf("turn reported errors %v, want the model to recover on its own", errs)
	}
}

// TestSendRefusesUnregisteredTool covers the plain hallucination: a name that
// belongs to no tool at all must not end the turn.
func TestSendRefusesUnregisteredTool(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "definitely_not_a_tool", `{}`),
		textResponse("my mistake"),
	}}
	agent := newTestAgent(t, []string{"shell", "browser", "filesystem"}, provider, &stubRunner{})

	events := runTurn(t, agent, "do something")

	results := toolResults(events)
	if len(results) != 1 || !strings.Contains(results[0].Err, "unknown tool") {
		t.Fatalf("tool results = %+v, want one unknown-tool rejection", results)
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Errorf("turn reported errors %v, want the model to recover on its own", errs)
	}
}

// A model that narrates before it acts ("let me check X") ends its message with
// the tool calls that text introduces. The message has to be announced there,
// between the two, or the TUI has nothing to hang the text on until the whole
// turn — every iteration of it — is over.
func TestAssistantMessageIsAnnouncedBeforeItsToolCalls(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		{
			Content:   "Let me list the workspace.",
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"ls"}`}},
			Usage:     llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
		textResponse("here is the listing"),
	}}
	agent := newTestAgent(t, []string{"shell"}, provider, &stubRunner{})

	var order []string
	for _, ev := range runTurn(t, agent, "list the workspace") {
		switch ev.(type) {
		case AssistantMessageMsg:
			order = append(order, "message")
		case ToolCallStartMsg:
			order = append(order, "tool")
		case AssistantDoneMsg:
			order = append(order, "done")
		}
	}

	want := []string{"message", "tool", "message", "done"}
	if !slices.Equal(order, want) {
		t.Errorf("event order = %v, want %v", order, want)
	}
}

// The operator watches a long tool chain against max_tool_iterations, so every
// request to the model is announced with its place in that allowance, before
// anything it streams.
func TestEachModelRequestIsAnnouncedWithItsNumber(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		{
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"ls"}`}},
			Usage:     llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
		textResponse("here is the listing"),
	}}
	agent := newTestAgent(t, []string{"shell"}, provider, &stubRunner{})

	var order []string
	for _, ev := range runTurn(t, agent, "list the workspace") {
		switch event := ev.(type) {
		case IterationStartMsg:
			order = append(order, fmt.Sprintf("iteration %d/%d", event.Iteration, event.MaxIterations))
		case AssistantMessageMsg:
			order = append(order, "message")
		case ToolCallStartMsg:
			order = append(order, "tool")
		}
	}

	want := []string{"iteration 1/5", "message", "tool", "iteration 2/5", "message"}
	if !slices.Equal(order, want) {
		t.Errorf("event order = %v, want %v", order, want)
	}
}
