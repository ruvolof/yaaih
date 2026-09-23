package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/governor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
)

func (p *stubProvider) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// A session resumed already past its limit must not be charged for one more
// request: the check comes before the first call of the turn, not after it.
func TestTurnOnASessionOverItsLimitSendsNothing(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{textResponse("should never be asked for")}}
	agent := newTestAgent(t, nil, provider, nil)
	agent.gov.SetLimits(governor.Limits{MaxCostUSD: 1})
	agent.gov.Seed(1000, 200, 1.25, true)

	events := runTurn(t, agent, "carry on")

	if count := provider.requestCount(); count != 0 {
		t.Errorf("provider was asked %d time(s), want no request once the limit is reached", count)
	}
	errs := errorMessages(events)
	if len(errs) != 1 || !strings.Contains(errs[0], "$1.00 limit") {
		t.Errorf("errors = %v, want one naming the cost limit", errs)
	}
	if done := assistantDone(events); len(done) != 1 || done[0].Cancelled {
		t.Errorf("assistant-done events = %+v, want exactly one not marked cancelled", done)
	}
}

// Crossing the limit mid-turn stops the next request, not the tool calls the
// model has already asked for: they cost nothing, and answering them leaves a
// history that can still be sent should the limit be raised.
func TestCrossingTheLimitMidTurnStopsBeforeTheNextRequest(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "shell_exec", `{"command":"ls"}`),
		textResponse("should never be asked for"),
	}}
	runner := &stubRunner{}
	agent := newTestAgent(t, []string{"shell"}, provider, runner)
	agent.gov.SetLimits(governor.Limits{MaxTokens: 15})

	events := runTurn(t, agent, "list the files")

	if count := provider.requestCount(); count != 1 {
		t.Errorf("provider was asked %d time(s), want only the request that crossed the limit", count)
	}
	if len(runner.executed()) != 1 {
		t.Errorf("executed = %v, want the tool call the model had already made", runner.executed())
	}
	errs := errorMessages(events)
	if len(errs) != 1 || !strings.Contains(errs[0], "15 limit") {
		t.Errorf("errors = %v, want one naming the token limit", errs)
	}
	history := agent.History()
	if last := history[len(history)-1]; last.Role != llm.RoleTool || last.ToolCallID != "call-1" {
		t.Errorf("history ends with %+v, want the answer to the model's tool call", last)
	}
}

// Compaction is a model call like any other and is billed like one.
func TestCompactIsRefusedOverTheLimit(t *testing.T) {
	provider := twoTurnsThenSummary("should never be asked for")
	agent, _, _ := newPersistedTestAgent(t, provider)
	agent.gov.SetLimits(governor.Limits{MaxTokens: 60})

	var before []llm.Message
	events := collectEvents(t, agent, func() {
		agent.Send(context.Background(), "deploy staging")
		agent.Send(context.Background(), "now the metrics")
		before = agent.History()
		agent.Compact(context.Background(), "")
	})

	if count := provider.requestCount(); count != 4 {
		t.Errorf("provider was asked %d time(s), want the four turn requests and no summary", count)
	}
	if after := agent.History(); len(after) != len(before) {
		t.Errorf("history went from %d to %d messages, want it untouched", len(before), len(after))
	}
	if errs := errorMessages(events); len(errs) != 1 || !strings.Contains(errs[0], "60 limit") {
		t.Errorf("errors = %v, want one naming the token limit", errs)
	}
}

// The limit is per session: /new starts one with nothing spent, under the same
// limit.
func TestANewSessionStartsUnderTheLimitAgain(t *testing.T) {
	provider := &stubProvider{responses: []*llm.ChatResponse{textResponse("fresh start")}}
	agent, _, _ := newPersistedTestAgent(t, provider)
	agent.gov.SetLimits(governor.Limits{MaxTokens: 100})
	agent.gov.Seed(90, 20, 0, false)

	events := collectEvents(t, agent, func() {
		if _, err := agent.NewSession(); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		agent.Send(context.Background(), "hello")
	})

	if count := provider.requestCount(); count != 1 {
		t.Errorf("provider was asked %d time(s), want the new session's turn to go through", count)
	}
	if errs := errorMessages(events); len(errs) != 0 {
		t.Errorf("errors = %v, want none in a fresh session", errs)
	}
	if limits := agent.SessionStats().Limits; limits.MaxTokens != 100 {
		t.Errorf("limits after /new = %+v, want the agent's limit kept", limits)
	}
}
