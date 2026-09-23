package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/governor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
)

// newOverBudgetAgent has a system prompt that alone exceeds the context
// budget: the window has nothing it is allowed to drop, so the first iteration
// gives up before the model is called.
func newOverBudgetAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := &config.Config{
		Model:        "stub-model",
		SystemPrompt: strings.Repeat("prompt ", 5000),
		Runtime:      config.RuntimeCfg{Workspace: t.TempDir()},
		Options:      windowedOptions(100),
	}
	return New(cfg, &stubProvider{}, governor.New(cfg.Model), governor.New(cfg.Model),
		nil, nil, NewGatekeeper(false), nil, nil, t.TempDir())
}

// A turn ends with exactly one AssistantDoneMsg however it ends. The UI reports
// itself as working from the moment Send is called until that event arrives —
// so a path that returns without it leaves the busy indicator spinning, the
// stop key pointed at a turn that is over, and /new, /resume and /reload
// refusing to run.
func TestEveryExitPathEndsTheTurnExactlyOnce(t *testing.T) {
	tests := []struct {
		name    string
		build   func(t *testing.T) *Agent
		wantErr string
	}{
		{
			name: "reply with no tool calls",
			build: func(t *testing.T) *Agent {
				provider := &stubProvider{responses: []*llm.ChatResponse{textResponse("all done")}}
				return newTestAgent(t, nil, provider, nil)
			},
		},
		{
			name: "the provider fails",
			build: func(t *testing.T) *Agent {
				return newTestAgent(t, nil, &stubProvider{}, nil)
			},
			wantErr: "llm error",
		},
		{
			name:    "the conversation no longer fits the budget",
			build:   newOverBudgetAgent,
			wantErr: "context budget",
		},
		{
			name: "the session is over its spending limit",
			build: func(t *testing.T) *Agent {
				agent := newTestAgent(t, nil, &stubProvider{}, nil)
				agent.gov.SetLimits(governor.Limits{MaxTokens: 10})
				agent.gov.Seed(10, 0, 0, false)
				return agent
			},
			wantErr: "limit",
		},
		{
			name: "the iteration cap is reached",
			build: func(t *testing.T) *Agent {
				provider := &stubProvider{responses: []*llm.ChatResponse{
					toolCallResponse("call-1", "shell_exec", `{"command":"ls"}`),
				}}
				return newTestAgentWithOptions(t, []string{"shell"}, provider, &stubRunner{}, config.Options{
					MaxToolIterations:  1,
					MaxToolResultBytes: testToolResultBudget,
					MaxFileReadLines:   50,
				})
			},
			wantErr: "max tool iterations",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := runTurn(t, tt.build(t), "do something")

			done := assistantDone(events)
			if len(done) != 1 || done[0].Cancelled {
				t.Fatalf("assistant-done events = %+v, want exactly one not marked cancelled", done)
			}
			errs := errorMessages(events)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Errorf("a turn that succeeded reported %v", errs)
					return
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0], tt.wantErr) {
				t.Errorf("errors = %v, want one mentioning %q", errs, tt.wantErr)
			}
		})
	}
}

// The busy rejection is the one event that does not end a turn: the turn it
// refers to is still running, and the UI has to keep showing it as such.
func TestASecondSendIsRejectedWithoutEndingTheFirstTurn(t *testing.T) {
	runner := newBlockingRunner()
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"sleep 300"}`),
	}}
	agent := newTestAgent(t, []string{"shell"}, provider, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := collectEvents(t, agent, func() {
		var turn sync.WaitGroup
		turn.Add(1)
		go func() {
			defer turn.Done()
			agent.Send(ctx, "sleep for a while")
		}()
		<-runner.entered
		agent.Send(ctx, "and another thing")
		cancel()
		turn.Wait()
	})

	done := assistantDone(events)
	if len(done) != 1 || !done[0].Cancelled {
		t.Fatalf("assistant-done events = %+v, want only the first turn's, marked cancelled", done)
	}
	errs := errorMessages(events)
	if len(errs) != 1 || !strings.Contains(errs[0], "still working") {
		t.Fatalf("errors = %v, want one saying the agent is still working", errs)
	}
	for _, msg := range agent.History() {
		if msg.Content == "and another thing" {
			t.Error("the rejected message was appended to the conversation anyway")
		}
	}
}

// Switching sessions replaces the conversation a running turn is writing into,
// so it is refused outright rather than trusting the UI's flag to have kept it
// from happening.
func TestSessionSwitchesAreRefusedWhileATurnRuns(t *testing.T) {
	runner := newBlockingRunner()
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"sleep 300"}`),
	}}
	agent, _, id := newPersistedTestAgentWithRunner(t, provider, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	collectEvents(t, agent, func() {
		var turn sync.WaitGroup
		turn.Add(1)
		go func() {
			defer turn.Done()
			agent.Send(ctx, "sleep for a while")
		}()
		<-runner.entered

		if _, err := agent.NewSession(); err == nil {
			t.Error("NewSession replaced the session underneath a running turn")
		}
		if _, err := agent.ResumeSession(id); err == nil {
			t.Error("ResumeSession replaced the conversation underneath a running turn")
		}

		cancel()
		turn.Wait()
	})

	if got := agent.CurrentSession(); got != id {
		t.Errorf("CurrentSession = %q, want the session the turn ran in %q", got, id)
	}
}

// Quitting mid-turn tears the agent down while the turn is still unwinding.
// Close waits for it: the turn still owns the tail of the transcript, and the
// event channel it is still publishing on is the one Close is about to shut.
func TestCloseWaitsForATurnStillUnwinding(t *testing.T) {
	runner := newBlockingRunner()
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"sleep 300"}`),
	}}
	agent, sessionsDir, id := newPersistedTestAgentWithRunner(t, provider, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go agent.Send(ctx, "sleep for a while")
	<-runner.entered

	// Nothing is draining the event stream from here on, the way nothing does
	// once the TUI has stopped: the turn has to be able to unwind regardless.
	cancel()
	agent.Close()

	_, msgs, _, err := session.Load(sessionsDir, id)
	if err != nil {
		t.Fatalf("load the transcript Close finalised: %v", err)
	}
	transcript := contentsOf(msgs)
	if !strings.Contains(transcript, "sleep for a while") {
		t.Errorf("the transcript lost the turn's user message: %v", roles(msgs))
	}
	if !strings.Contains(transcript, "cancelled by operator") {
		t.Errorf("the session was finalised before the turn answered its tool call: %v", roles(msgs))
	}
	if dangling := danglingToolCalls(msgs); len(dangling) != 0 {
		t.Errorf("the persisted transcript leaves %v unanswered", dangling)
	}
}
