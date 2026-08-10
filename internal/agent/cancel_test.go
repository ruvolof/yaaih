package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/llm"
)

// blockingRunner holds the turn inside the tool call until its context is
// cancelled — a stand-in for the long shell command the operator interrupts.
// It records what reached it so a test can assert the calls after the cancel
// were never dispatched.
type blockingRunner struct {
	entered chan struct{}
	once    sync.Once

	mu       sync.Mutex
	commands []string
}

func newBlockingRunner() *blockingRunner {
	return &blockingRunner{entered: make(chan struct{})}
}

func (r *blockingRunner) Execute(ctx context.Context, cmd string) (string, error) {
	r.mu.Lock()
	r.commands = append(r.commands, cmd)
	r.mu.Unlock()
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return "", ctx.Err()
}

func (r *blockingRunner) CWD() string     { return "/workspace" }
func (r *blockingRunner) IsRunning() bool { return true }
func (r *blockingRunner) Close() error    { return nil }

func (r *blockingRunner) executed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

// streamThenBlockProvider emits one chunk and then holds the stream open until
// its context is cancelled — the shape of a stop pressed while the reply is
// still arriving. It returns no response, as a real aborted stream does.
type streamThenBlockProvider struct {
	chunk   string
	entered chan struct{}
}

func (p *streamThenBlockProvider) Name() string { return "stub" }

func (p *streamThenBlockProvider) StreamChat(ctx context.Context, req *llm.ChatRequest, cb llm.StreamCallback) (*llm.ChatResponse, error) {
	cb(llm.StreamEvent{DeltaText: p.chunk})
	close(p.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// cancelWhenEntered cancels ctx once the turn signals it has reached the point
// the test wants to interrupt.
func cancelWhenEntered(entered <-chan struct{}, cancel context.CancelFunc) {
	go func() {
		<-entered
		cancel()
	}()
}

// danglingToolCalls returns the ids of tool calls the history never answers.
// Providers reject a request that contains one, so a turn ending early leaves
// the session unusable unless every call it started has a reply.
func danglingToolCalls(history []llm.Message) []string {
	answered := make(map[string]bool)
	for _, msg := range history {
		if msg.Role == llm.RoleTool {
			answered[msg.ToolCallID] = true
		}
	}
	var dangling []string
	for _, msg := range history {
		for _, tc := range msg.ToolCalls {
			if !answered[tc.ID] {
				dangling = append(dangling, tc.ID)
			}
		}
	}
	return dangling
}

func assistantDone(events []Event) []AssistantDoneMsg {
	var out []AssistantDoneMsg
	for _, ev := range events {
		if msg, ok := ev.(AssistantDoneMsg); ok {
			out = append(out, msg)
		}
	}
	return out
}

func toolReplies(history []llm.Message) []llm.Message {
	var out []llm.Message
	for _, msg := range history {
		if msg.Role == llm.RoleTool {
			out = append(out, msg)
		}
	}
	return out
}

// TestCancelDuringToolAnswersEveryUnfinishedCall is D1's core invariant. The
// model asks for three shell calls, the operator stops the turn while the
// first is running, and all three must come back answered — the two that never
// ran included, since an unanswered tool_call poisons every later turn, not
// just this one.
func TestCancelDuringToolAnswersEveryUnfinishedCall(t *testing.T) {
	runner := newBlockingRunner()
	provider := &stubProvider{responses: []*llm.ChatResponse{{
		ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"sleep 300"}`},
			{ID: "call-2", Name: "shell_exec", Arguments: `{"command":"echo two"}`},
			{ID: "call-3", Name: "shell_exec", Arguments: `{"command":"echo three"}`},
		},
		Usage: llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}}}
	agent := newTestAgent(t, []string{"shell"}, provider, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelWhenEntered(runner.entered, cancel)

	events := runTurnCtx(t, agent, ctx, "sleep for a while")

	if dangling := danglingToolCalls(agent.History()); len(dangling) != 0 {
		t.Errorf("tool calls %v left unanswered; the next turn would be rejected by the provider", dangling)
	}
	replies := toolReplies(agent.History())
	if len(replies) != 3 {
		t.Fatalf("tool replies = %d, want 3", len(replies))
	}
	for _, reply := range replies {
		if !strings.Contains(reply.Content, "cancelled by operator") {
			t.Errorf("reply to %s = %q, want it to say the operator cancelled", reply.ToolCallID, reply.Content)
		}
	}

	if commands := runner.executed(); len(commands) != 1 {
		t.Errorf("executor ran %v, want only the call that was already in flight", commands)
	}

	done := assistantDone(events)
	if len(done) != 1 || !done[0].Cancelled {
		t.Fatalf("assistant-done events = %+v, want exactly one marked cancelled", done)
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Errorf("cancelling reported errors %v; stopping a turn is not a failure", errs)
	}
}

// TestCancelDuringStreamKeepsPartialReply pins that a reply interrupted
// mid-sentence stays in the history. The TUI has already shown it, so dropping
// it would leave the visible transcript asserting something the next request
// denies.
func TestCancelDuringStreamKeepsPartialReply(t *testing.T) {
	provider := &streamThenBlockProvider{chunk: "here is what I fou", entered: make(chan struct{})}
	agent := newTestAgent(t, []string{"shell"}, provider, &stubRunner{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelWhenEntered(provider.entered, cancel)

	events := runTurnCtx(t, agent, ctx, "find something")

	history := agent.History()
	last := history[len(history)-1]
	if last.Role != llm.RoleAssistant || last.Content != provider.chunk {
		t.Fatalf("last message = %+v, want the partial assistant reply %q", last, provider.chunk)
	}

	done := assistantDone(events)
	if len(done) != 1 || !done[0].Cancelled {
		t.Fatalf("assistant-done events = %+v, want exactly one marked cancelled", done)
	}
	// A cancelled stream surfaces as "llm error: context canceled" if the
	// cancellation is not recognised for what it is.
	if errs := errorMessages(events); len(errs) != 0 {
		t.Errorf("cancelling reported errors %v; stopping a turn is not a failure", errs)
	}
}

// TestCancelBeforeFirstCallSkipsTheModel covers a stop that lands between the
// user pressing enter and the request going out: no provider call should
// happen at all.
func TestCancelBeforeFirstCallSkipsTheModel(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{textResponse("should never be sent")}}
	agent := newTestAgent(t, []string{"shell"}, provider, &stubRunner{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events := runTurnCtx(t, agent, ctx, "never mind")

	if req := provider.request(0); req != nil {
		t.Error("the model was called for a turn that was already cancelled")
	}
	done := assistantDone(events)
	if len(done) != 1 || !done[0].Cancelled {
		t.Fatalf("assistant-done events = %+v, want exactly one marked cancelled", done)
	}
}

// TestUncancelledTurnIsNotMarkedCancelled is the control: the TUI keys its
// "turn cancelled" marker off this flag, so an ordinary turn must not set it.
func TestUncancelledTurnIsNotMarkedCancelled(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"ls"}`),
		textResponse("here is the listing"),
	}}
	agent := newTestAgent(t, []string{"shell"}, provider, &stubRunner{})

	events := runTurn(t, agent, "list the workspace")

	done := assistantDone(events)
	if len(done) != 1 || done[0].Cancelled {
		t.Fatalf("assistant-done events = %+v, want exactly one not marked cancelled", done)
	}
}
