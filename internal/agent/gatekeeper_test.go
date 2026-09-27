package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/executor"
	"github.com/ruvolof/aibitat-harness-go/internal/governor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
)

func newGatedTestAgent(t *testing.T, gate *Gatekeeper, prov llm.Provider, runner executor.Executor) *Agent {
	t.Helper()
	cfg := &config.Config{
		Model:        "stub-model",
		Capabilities: []string{"shell"},
		Runtime:      config.RuntimeCfg{Workspace: t.TempDir()},
		Options: config.Options{
			MaxToolIterations:  5,
			MaxToolResultBytes: testToolResultBudget,
			MaxFileReadLines:   50,
		},
	}
	return New(cfg, prov, governor.New(cfg.Model), governor.New(cfg.Model),
		runner, nil, gate, nil, session.Transcript{}, t.TempDir())
}

// runTurnWithOperator drives a turn with an operator standing at the approval
// modal, answering every prompt with answer.
func runTurnWithOperator(t *testing.T, ag *Agent, input string, answer func(*ApprovalRequest)) []Event {
	t.Helper()
	collected := make(chan []Event, 1)
	go func() {
		var events []Event
		for ev := range ag.Events() {
			events = append(events, ev)
			if msg, ok := ev.(ApprovalRequestMsg); ok {
				answer(msg.Request)
			}
		}
		collected <- events
	}()

	ag.Send(context.Background(), input)
	ag.Close()

	select {
	case events := <-collected:
		return events
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the turn to finish")
		return nil
	}
}

// An agent whose safety policy asks nothing must not stall on a channel nobody
// is reading — the TUI only reads it when approval is configured.
func TestRequestSkipsThePromptWhenApprovalIsNotRequired(t *testing.T) {
	gate := NewGatekeeper(false)

	decision, err := gate.Request(context.Background(), "shell_exec", `{"command":"ls"}`)

	if err != nil || decision != ApprovalApprove {
		t.Fatalf("decision = %v, err = %v, want an immediate approval", decision, err)
	}
	select {
	case req := <-gate.Requests():
		t.Errorf("a prompt for %q was emitted although approval is not required", req.ToolName)
	default:
	}
}

func TestRequestReturnsTheOperatorsDecision(t *testing.T) {
	cases := []struct {
		name   string
		answer func(*ApprovalRequest)
		want   ApprovalDecision
	}{
		{"approve", func(req *ApprovalRequest) { req.Approve() }, ApprovalApprove},
		{"deny", func(req *ApprovalRequest) { req.Deny() }, ApprovalDeny},
		{"cancel", func(req *ApprovalRequest) { req.Cancel() }, ApprovalCancel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewGatekeeper(true)
			go func() {
				req := <-gate.Requests()
				if req.ToolName != "shell_exec" {
					t.Errorf("prompt names %q, want shell_exec", req.ToolName)
				}
				if !strings.Contains(req.Args, "rm -rf /") {
					t.Errorf("prompt args = %q, want the command the operator is judging", req.Args)
				}
				tc.answer(req)
			}()

			decision, err := gate.Request(context.Background(), "shell_exec", `{"command":"rm -rf /"}`)
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if decision != tc.want {
				t.Errorf("decision = %v, want %v", decision, tc.want)
			}
		})
	}
}

func TestAutoApproveStopsPrompting(t *testing.T) {
	gate := NewGatekeeper(true)
	gate.SetAutoApprove(true)

	decision, err := gate.Request(context.Background(), "shell_exec", `{"command":"ls"}`)
	if err != nil || decision != ApprovalApprove {
		t.Fatalf("decision = %v, err = %v, want an immediate approval", decision, err)
	}
	select {
	case req := <-gate.Requests():
		t.Errorf("a prompt for %q was emitted while auto-approve is on", req.ToolName)
	default:
	}

	// Reversible: turning it back off restores the prompt.
	gate.SetAutoApprove(false)
	go func() {
		req := <-gate.Requests()
		req.Deny()
	}()
	if decision, _ := gate.Request(context.Background(), "shell_exec", "{}"); decision != ApprovalDeny {
		t.Errorf("decision = %v, want the prompt back after auto-approve was turned off", decision)
	}
}

// Always allowing one tool waves that tool through for the rest of the session
// and nothing else: a shell the operator trusts says nothing about a browser.
func TestApproveToolStopsPromptingForThatToolOnly(t *testing.T) {
	gate := NewGatekeeper(true)
	go func() { (<-gate.Requests()).ApproveTool() }()
	if decision, _ := gate.Request(context.Background(), "shell_exec", "{}"); decision != ApprovalApprove {
		t.Fatalf("decision = %v, want the call that was always-allowed approved", decision)
	}

	if decision, err := gate.Request(context.Background(), "shell_exec", "{}"); err != nil || decision != ApprovalApprove {
		t.Fatalf("decision = %v, err = %v, want an immediate approval", decision, err)
	}
	select {
	case req := <-gate.Requests():
		t.Errorf("a prompt for %q was emitted after it was always-allowed", req.ToolName)
	default:
	}

	go func() { (<-gate.Requests()).Deny() }()
	if decision, _ := gate.Request(context.Background(), "browser_eval", "{}"); decision != ApprovalDeny {
		t.Errorf("decision = %v, want another tool still put to the operator", decision)
	}

	standing := gate.Standing()
	if standing.All || !slices.Equal(standing.Tools, []string{"shell_exec"}) {
		t.Errorf("standing = %+v, want shell_exec alone allowed", standing)
	}
}

func TestApproveAllStopsPromptingForEveryTool(t *testing.T) {
	gate := NewGatekeeper(true)
	go func() { (<-gate.Requests()).ApproveAll() }()
	if decision, _ := gate.Request(context.Background(), "shell_exec", "{}"); decision != ApprovalApprove {
		t.Fatalf("decision = %v, want the call approved", decision)
	}

	for _, toolName := range []string{"shell_exec", "file_write", "browser_eval"} {
		if decision, _ := gate.Request(context.Background(), toolName, "{}"); decision != ApprovalApprove {
			t.Errorf("%s: decision = %v, want an immediate approval", toolName, decision)
		}
	}
	select {
	case req := <-gate.Requests():
		t.Errorf("a prompt for %q was emitted after everything was allowed", req.ToolName)
	default:
	}
	if standing := gate.Standing(); !standing.All || !standing.Required {
		t.Errorf("standing = %+v, want every tool shown as allowed", standing)
	}
}

// The chain the operator is sitting through is the case this exists for: once a
// tool is always-allowed, the rest of the batch runs without coming back.
func TestAlwaysAllowCoversTheRestOfTheBatch(t *testing.T) {
	runner := &stubRunner{}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		{
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"one"}`},
				{ID: "call-2", Name: "shell_exec", Arguments: `{"command":"two"}`},
				{ID: "call-3", Name: "shell_exec", Arguments: `{"command":"three"}`},
			},
			Usage: llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
		textResponse("done"),
	}}
	agent := newGatedTestAgent(t, NewGatekeeper(true), provider, runner)

	prompts := 0
	runTurnWithOperator(t, agent, "run the batch", func(req *ApprovalRequest) {
		prompts++
		req.ApproveTool()
	})

	if prompts != 1 {
		t.Errorf("operator was asked %d times, want once", prompts)
	}
	if commands := runner.executed(); !slices.Equal(commands, []string{"one", "two", "three"}) {
		t.Errorf("executor ran %v, want the whole batch", commands)
	}
}

// What the operator waved through was judged against one conversation.
func TestANewSessionAsksAgain(t *testing.T) {
	gate := NewGatekeeper(true)
	agent := newGatedTestAgent(t, gate, &stubProvider{}, &stubRunner{})
	t.Cleanup(agent.Close)
	gate.AllowTool("shell_exec")
	gate.SetAutoApprove(true)

	if _, err := agent.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if standing := agent.ApprovalStanding(); standing.All || len(standing.Tools) != 0 {
		t.Errorf("standing = %+v after a new session, want nothing allowed", standing)
	}
}

// Stopping the turn while the modal is open has to release the tool call, or
// the turn hangs on an operator who has already moved on.
func TestRequestGivesUpWhenTheTurnIsCancelled(t *testing.T) {
	gate := NewGatekeeper(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-gate.Requests()
		cancel()
	}()

	decision, err := gate.Request(ctx, "shell_exec", "{}")

	if err == nil {
		t.Fatal("Request returned no error for a cancelled turn")
	}
	if decision != ApprovalDeny {
		t.Errorf("decision = %v, want the call refused when nobody answered", decision)
	}
}

func TestGatekeeperCloseIsIdempotent(t *testing.T) {
	gate := NewGatekeeper(true)
	gate.Close()
	gate.Close()

	if _, open := <-gate.Requests(); open {
		t.Error("Requests() still delivers after Close")
	}
}

// Denying one call refuses that call only. The model has to be told, in a reply
// to the call it made, so it can try something else on the next iteration.
func TestDeniedToolIsAnsweredAndTheTurnContinues(t *testing.T) {
	runner := &stubRunner{}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"rm -rf /"}`),
		textResponse("understood, I will not do that"),
	}}
	agent := newGatedTestAgent(t, NewGatekeeper(true), provider, runner)

	events := runTurnWithOperator(t, agent, "clean up the disk", func(req *ApprovalRequest) {
		req.Deny()
	})

	if commands := runner.executed(); len(commands) != 0 {
		t.Errorf("executor ran %v for a denied call", commands)
	}
	results := toolResults(events)
	if len(results) != 1 || !strings.Contains(results[0].Err, "denied") {
		t.Fatalf("tool results = %+v, want one denial", results)
	}

	second := provider.request(1)
	if second == nil {
		t.Fatal("the turn ended without letting the model react to the denial")
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != llm.RoleTool || last.ToolCallID != "call-1" {
		t.Fatalf("last message = %+v, want a tool reply to call-1", last)
	}
	if dangling := danglingToolCalls(agent.History()); len(dangling) != 0 {
		t.Errorf("tool calls %v left unanswered", dangling)
	}
}

// Cancelling at the modal ends the turn. Every call in the batch still has to
// be answered — the history it leaves behind is what the next turn sends, and a
// tool_call without its reply is a provider 400.
func TestCancelAtTheModalEndsTheTurnAndAnswersTheWholeBatch(t *testing.T) {
	runner := &stubRunner{}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		{
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"one"}`},
				{ID: "call-2", Name: "shell_exec", Arguments: `{"command":"two"}`},
				{ID: "call-3", Name: "shell_exec", Arguments: `{"command":"three"}`},
			},
			Usage: llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
	}}
	agent := newGatedTestAgent(t, NewGatekeeper(true), provider, runner)

	events := runTurnWithOperator(t, agent, "run the batch", func(req *ApprovalRequest) {
		req.Cancel()
	})

	if commands := runner.executed(); len(commands) != 0 {
		t.Errorf("executor ran %v after the turn was cancelled", commands)
	}
	if dangling := danglingToolCalls(agent.History()); len(dangling) != 0 {
		t.Errorf("tool calls %v left unanswered", dangling)
	}
	done := assistantDone(events)
	if len(done) != 1 || !done[0].Cancelled {
		t.Errorf("AssistantDoneMsg = %+v, want exactly one marked cancelled", done)
	}
	if provider.request(1) != nil {
		t.Error("the model was given another iteration after the operator cancelled")
	}
}

// The positive control: approving lets the call through, so the tests above
// cannot pass by blocking everything.
func TestApprovedToolReachesTheExecutor(t *testing.T) {
	runner := &stubRunner{}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"ls -la"}`),
		textResponse("here is the listing"),
	}}
	agent := newGatedTestAgent(t, NewGatekeeper(true), provider, runner)

	events := runTurnWithOperator(t, agent, "list the workspace", func(req *ApprovalRequest) {
		req.Approve()
	})

	if commands := runner.executed(); len(commands) != 1 || commands[0] != "ls -la" {
		t.Fatalf("executor ran %v, want [ls -la]", commands)
	}
	if results := toolResults(events); len(results) != 1 || results[0].Err != "" {
		t.Errorf("tool results = %+v, want one successful result", results)
	}
}
