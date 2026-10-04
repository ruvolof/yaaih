package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/llm"
)

// bulkOutput is a tool result big enough to matter to the budget. The lines
// have to differ: identical ones are collapsed to a single line and a count
// before the result is ever recorded, which would leave the fixture too small
// to reach any budget at all.
func bulkOutput(lines int) string {
	var b strings.Builder
	for line := 0; line < lines; line++ {
		fmt.Fprintf(&b, "log entry %d: something happened worth writing down\n", line)
	}
	return b.String()
}

func windowedOptions(maxContextTokens int) config.Options {
	return config.Options{
		MaxToolIterations:  6,
		MaxToolResultBytes: 64 << 10,
		MaxFileReadLines:   50,
		MaxContextTokens:   maxContextTokens,
	}
}

func historyTrimmed(events []Event) []HistoryTrimmedMsg {
	var out []HistoryTrimmedMsg
	for _, ev := range events {
		if msg, ok := ev.(HistoryTrimmedMsg); ok {
			out = append(out, msg)
		}
	}
	return out
}

func containsUserMessage(msgs []llm.Message, content string) bool {
	for _, msg := range msgs {
		if msg.Role == llm.RoleUser && msg.Content == content {
			return true
		}
	}
	return false
}

// A turn whose own tool output crosses the budget has to keep going. The window
// runs between iterations, so what it takes is older exchanges — never the
// instruction being worked on or the results just received.
func TestWindowedTurnKeepsWorkingAndStaysValid(t *testing.T) {
	runner := &stubRunner{output: bulkOutput(120)}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"one"}`),
		toolCallResponse("call-2", "shell_exec", `{"command":"two"}`),
		toolCallResponse("call-3", "shell_exec", `{"command":"three"}`),
		toolCallResponse("call-4", "shell_exec", `{"command":"four"}`),
		textResponse("done"),
	}}
	agent := newTestAgentWithOptions(t, []string{"shell"}, provider, runner, windowedOptions(2000))

	events := runTurn(t, agent, "gather the logs")

	if commands := runner.executed(); len(commands) != 4 {
		t.Fatalf("executor ran %v, want all four commands — the turn stopped early", commands)
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Fatalf("turn reported errors %v", errs)
	}

	for i := 0; ; i++ {
		req := provider.request(i)
		if req == nil {
			break
		}
		if dangling := danglingToolCalls(req.Messages); len(dangling) != 0 {
			t.Errorf("request %d carried unanswered tool calls %v", i, dangling)
		}
		if req.Messages[0].Role != llm.RoleSystem {
			t.Errorf("request %d opens with %s, want the system prompt", i, req.Messages[0].Role)
		}
		if !containsUserMessage(req.Messages, "gather the logs") {
			t.Errorf("request %d lost the instruction the turn is working on", i)
		}
	}

	last := provider.request(4)
	if last == nil {
		t.Fatal("the turn never reached its final iteration")
	}
	tail := last.Messages[len(last.Messages)-1]
	if tail.Role != llm.RoleTool || tail.ToolCallID != "call-4" {
		t.Errorf("last message = %+v, want the result the model is about to read", tail)
	}
	if trims := historyTrimmed(events); len(trims) == 0 {
		t.Error("the operator was never told the conversation had been trimmed")
	}
}

// The window is a per-request view. What is stored — and what a resumed session
// will load — stays whole.
func TestWindowingLeavesTheStoredHistoryComplete(t *testing.T) {
	runner := &stubRunner{output: bulkOutput(120)}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"one"}`),
		textResponse("first done"),
		toolCallResponse("call-2", "shell_exec", `{"command":"two"}`),
		textResponse("second done"),
		toolCallResponse("call-3", "shell_exec", `{"command":"three"}`),
		textResponse("third done"),
	}}
	agent := newTestAgentWithOptions(t, []string{"shell"}, provider, runner, windowedOptions(2000))

	runTurns(t, agent, "gather the logs", "and the metrics", "and the traces")

	history := agent.History()
	if len(history) != 13 {
		t.Fatalf("stored history has %d messages, want the system prompt and three whole turns", len(history))
	}
	for _, msg := range history {
		if msg.Role == llm.RoleTool && strings.Contains(msg.Content, "context budget") {
			t.Error("a result the window shortened was written back into the stored conversation")
		}
	}

	// The final request goes out before the reply it produces is recorded, so
	// everything stored but that reply was on offer to be sent.
	sent := provider.request(5)
	if sent == nil {
		t.Fatal("the third turn never reached its final iteration")
	}
	if len(sent.Messages) >= len(history)-1 {
		t.Errorf("the last request carried %d messages against %d stored — nothing was windowed",
			len(sent.Messages), len(history))
	}
	if !containsUserMessage(sent.Messages, "and the traces") {
		t.Error("the window dropped the question the last turn was answering")
	}
	if containsUserMessage(sent.Messages, "gather the logs") {
		t.Error("the oldest turn was still being sent well past the budget")
	}
}

// A budget so small that even the system prompt and the turn in flight cannot
// fit has nothing left to give. Paying for a request the provider will reject
// helps nobody, so the turn ends and says which knob to turn.
func TestTurnAbortsWhenNothingIsLeftToDrop(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{textResponse("never reached")}}
	agent := newTestAgentWithOptions(t, nil, provider, nil, windowedOptions(20))

	events := runTurn(t, agent, strings.Repeat("a very long question ", 200))

	if provider.request(0) != nil {
		t.Error("a request that cannot fit was sent to the provider anyway")
	}
	errs := errorMessages(events)
	if len(errs) != 1 || !strings.Contains(errs[0], "max_context_tokens") {
		t.Fatalf("errors = %v, want one naming the option that fixes it", errs)
	}
	if dangling := danglingToolCalls(agent.History()); len(dangling) != 0 {
		t.Errorf("the abandoned turn left %v behind, so no later turn can be sent either", dangling)
	}
}

func TestUnwindowedAgentSendsTheWholeConversation(t *testing.T) {
	runner := &stubRunner{output: bulkOutput(120)}
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"one"}`),
		toolCallResponse("call-2", "shell_exec", `{"command":"two"}`),
		textResponse("done"),
	}}
	agent := newTestAgentWithOptions(t, []string{"shell"}, provider, runner, windowedOptions(0))

	events := runTurn(t, agent, "gather the logs")

	last := provider.request(2)
	if last == nil {
		t.Fatal("the turn never reached its final iteration")
	}
	if stored := agent.History(); len(last.Messages) != len(stored)-1 {
		t.Errorf("last request carried %d messages against %d stored (less the reply it produced), with windowing disabled",
			len(last.Messages), len(stored))
	}
	if trims := historyTrimmed(events); len(trims) != 0 {
		t.Errorf("windowing is disabled but reported %+v", trims)
	}
}
