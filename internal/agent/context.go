// The context window: how much of the conversation each request carries.
//
// The whole history is re-sent on every iteration, so left alone it grows until
// it no longer fits the model — and from that point on every turn fails, not
// just the one that got too big. What is stored stays complete; what is sent is
// windowed.
package agent

import (
	"github.com/ruvolof/aibitat-harness-go/internal/history"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
)

// requestMessages returns the conversation to send for one iteration: the whole
// history while it fits the budget, and a window over it once it does not.
//
// Called at the top of every iteration rather than once per turn, because a
// turn's own tool results are usually what crosses the line. The window never
// reaches the turn in flight, so crossing it mid-turn costs older exchanges and
// leaves the current one whole.
func (a *Agent) requestMessages(tools []llm.ToolDefinition) ([]llm.Message, history.Stats) {
	a.mu.Lock()
	messages := append([]llm.Message(nil), a.messages...)
	windowedThrough := a.dropThrough
	shrunkBefore := a.shrunkResults
	calibration := a.calibration
	a.mu.Unlock()

	budget := calibration.Bound(history.BudgetFor(a.cfg.Options.MaxContextTokens))
	windowed, stats := history.Window(messages, tools, budget, windowedThrough)

	a.mu.Lock()
	a.dropThrough = stats.DropThrough
	a.shrunkResults = stats.ShrunkResults
	a.mu.Unlock()

	// Every iteration re-reduces the same conversation, so the interesting
	// event is the window moving or another result going — not the reductions
	// already reported, which would otherwise be announced again on every
	// iteration of a long turn.
	if !stats.Advanced(windowedThrough) && stats.ShrunkResults <= shrunkBefore {
		return windowed, stats
	}
	log.Info("conversation windowed",
		"dropped_messages", stats.DroppedMessages,
		"shrunk_results", stats.ShrunkResults,
		"tokens_before", stats.BeforeTokens,
		"tokens_after", stats.AfterTokens,
		"budget", a.cfg.Options.MaxContextTokens,
		"over_budget", stats.OverBudget,
	)
	a.publish(HistoryTrimmedMsg{
		DroppedMessages: stats.DroppedMessages,
		ShrunkResults:   stats.ShrunkResults,
		BeforeTokens:    calibration.Apply(stats.BeforeTokens),
		AfterTokens:     calibration.Apply(stats.AfterTokens),
	})
	return windowed, stats
}

// calibrate corrects the estimator against the prompt token count the provider
// billed for the request just sent, which is the only authoritative measure of
// a prompt the harness has.
func (a *Agent) calibrate(estimated, reported int) {
	a.mu.Lock()
	before := a.calibration
	a.calibration = before.Observe(estimated, reported)
	after := a.calibration
	a.mu.Unlock()

	if after.Scale() != before.Scale() {
		log.Debug("prompt estimate calibrated",
			"estimated", estimated,
			"reported", reported,
			"scale", after.Scale(),
		)
	}
}

// promptTokens is what the operator is shown for the request just sent:
// the provider's own count when it gave one, the corrected estimate otherwise.
func (a *Agent) promptTokens(stats history.Stats, reported int) int {
	if reported > 0 {
		return reported
	}
	return a.calibratedTokens(stats.AfterTokens)
}

// statsUpdate builds the sidebar refresh. Everything that moves the numbers
// goes through here, so no caller can leave the context figures out and blank
// the gauge by accident.
func (a *Agent) statsUpdate(contextTokens int) StatsUpdateMsg {
	return StatsUpdateMsg{
		Session:       a.gov.Snapshot(),
		Global:        a.globGov.Snapshot(),
		CWD:           a.currentCWD(),
		ContextTokens: contextTokens,
		ContextBudget: a.cfg.Options.MaxContextTokens,
	}
}

// calibratedTokens restates an estimate in the units the operator configured
// the budget in.
func (a *Agent) calibratedTokens(estimate int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calibration.Apply(estimate)
}
