package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/governor"
)

// The gauge is the operator's warning that a compaction is coming. A stats
// refresh that forgets the context figures would blank it silently.
func TestContextFillIsKeptForTheSidebar(t *testing.T) {
	m := &Model{}
	m.noteContextFill(4000, 64_000)

	if m.contextTokens != 4000 || m.contextBudget != 64_000 {
		t.Errorf("context = %d/%d, want 4000/64000", m.contextTokens, m.contextBudget)
	}
	if !strings.Contains(m.renderSidebar(), "Context") {
		t.Error("the sidebar has no Context section after a stats update")
	}
}

// Meeting the ceiling as a failed turn is a bad way to learn the context was
// filling up.
func TestContextWarningAppearsOnceThenRearms(t *testing.T) {
	m := &Model{}

	m.noteContextFill(4000, 64_000) // 6%
	if body := m.chatBuf.String(); body != "" {
		t.Fatalf("warned at 6%% full: %q", stripANSI(body))
	}

	m.noteContextFill(58_000, 64_000) // 90%
	warning := stripANSI(m.chatBuf.String())
	if !strings.Contains(warning, "90% full") || !strings.Contains(warning, "/compact") {
		t.Fatalf("warning = %q, want the fill level and what to do about it", warning)
	}

	m.noteContextFill(60_000, 64_000) // 93%, still the same filling
	if got := strings.Count(stripANSI(m.chatBuf.String()), "/compact"); got != 1 {
		t.Errorf("warned %d times during one filling, want 1", got)
	}

	// A /compact drops the fill; the next filling deserves its own warning.
	m.noteContextFill(9_000, 64_000)
	m.noteContextFill(59_000, 64_000)
	if got := strings.Count(stripANSI(m.chatBuf.String()), "/compact"); got != 2 {
		t.Errorf("warned %d times across two fillings, want 2", got)
	}
}

func TestContextWarningStaysQuietWithoutABudget(t *testing.T) {
	m := &Model{}
	m.noteContextFill(0, 0)
	m.noteContextFill(5_000, 0)

	if body := m.chatBuf.String(); body != "" {
		t.Errorf("warned with windowing disabled: %q", stripANSI(body))
	}
	if strings.Contains(m.renderSidebar(), "Context") {
		t.Error("the sidebar shows a context gauge with no budget to measure against")
	}
}

func limitedStats(tokens int, cost float64, costAvailable bool) governor.Stats {
	return governor.Stats{
		TotalTokens:      tokens,
		EstimatedCostUSD: cost,
		CostAvailable:    costAvailable,
		Limits:           governor.Limits{MaxCostUSD: 1, MaxTokens: 100_000},
	}
}

// A limit reached without warning is a long autonomous run stopping mid-task
// for no visible reason.
func TestSpendWarningAppearsOnceThenRearmsForTheNextSession(t *testing.T) {
	m := &Model{}

	m.noteSpend(limitedStats(10_000, 0.5, true))
	if body := m.chatBuf.String(); body != "" {
		t.Fatalf("warned at half the limit: %q", stripANSI(body))
	}

	m.noteSpend(limitedStats(10_000, 0.85, true))
	warning := stripANSI(m.chatBuf.String())
	if !strings.Contains(warning, "85%") || !strings.Contains(warning, "/new") {
		t.Fatalf("warning = %q, want how close the limit is and what to do about it", warning)
	}

	m.noteSpend(limitedStats(95_000, 0.9, true))
	if got := strings.Count(stripANSI(m.chatBuf.String()), "spending limit"); got != 1 {
		t.Errorf("warned %d times approaching one limit, want 1", got)
	}

	// /new starts from nothing; that session's approach deserves its own warning.
	m.noteSpend(limitedStats(0, 0, true))
	m.noteSpend(limitedStats(90_000, 0.1, true))
	if got := strings.Count(stripANSI(m.chatBuf.String()), "spending limit"); got != 2 {
		t.Errorf("warned %d times across two sessions, want 2", got)
	}
}

func TestUnenforceableCostLimitIsReportedOnce(t *testing.T) {
	m := &Model{}
	m.noteSpend(limitedStats(100, 0, false))
	m.noteSpend(limitedStats(200, 0, false))

	body := stripANSI(m.chatBuf.String())
	if got := strings.Count(body, "cannot be enforced"); got != 1 {
		t.Errorf("warned %d times about an unenforced cost limit, want 1: %q", got, body)
	}
}

func TestSidebarShowsSpendAgainstItsLimits(t *testing.T) {
	m := &Model{sessionStats: limitedStats(12_345, 0.25, true)}

	sidebar := stripANSI(m.renderSidebar())
	for _, want := range []string{"tot: 12345 / 100k", "$0.2500 / $1.00"} {
		if !strings.Contains(sidebar, want) {
			t.Errorf("sidebar is missing %q:\n%s", want, sidebar)
		}
	}

	m.sessionStats = governor.Stats{TotalTokens: 12_345}
	sidebar = stripANSI(m.renderSidebar())
	if strings.Contains(sidebar, " / ") {
		t.Errorf("sidebar shows a limit for a session without one:\n%s", sidebar)
	}
}

// Streamed text lives below the committed transcript until something commits
// it. Across a tool call that means narration sinking under the tool traffic it
// introduced, and staying pinned to the bottom of the view for the rest of the
// turn.
func TestNarrationIsCommittedAboveTheToolCallItIntroduced(t *testing.T) {
	m := &Model{}

	m.pendingAs.WriteString("Let me list the workspace.")
	m.commitAssistant()
	m.appendLine(styleTool.Render("→ shell_exec {\"command\":\"ls\"}"))
	m.appendLine(styleToolRes.Render("  ✓ agents"))
	m.pendingAs.WriteString("There are three agents.")
	m.commitAssistant()

	transcript := strings.Split(stripANSI(m.chatBuf.String()), "\n")
	want := []string{
		"Let me list the workspace.",
		"→ shell_exec {\"command\":\"ls\"}",
		"  ✓ agents",
		"There are three agents.",
	}
	if !slices.Equal(transcript, want) {
		t.Errorf("transcript =\n%q\nwant\n%q", transcript, want)
	}
	if m.pendingAs.Len() != 0 {
		t.Errorf("pendingAs still holds %q after committing", m.pendingAs.String())
	}
	if m.lastAssistant != "There are three agents." {
		t.Errorf("lastAssistant = %q, want the most recent message", m.lastAssistant)
	}
}

// Committing nothing must not push a blank line into the transcript: the turn's
// last message is normally committed before AssistantDoneMsg reaches the model.
func TestCommittingAnEmptyReplyChangesNothing(t *testing.T) {
	m := &Model{}
	m.appendLine("only line")
	m.commitAssistant()

	if body := stripANSI(m.chatBuf.String()); body != "only line" {
		t.Errorf("transcript = %q, want it untouched", body)
	}
}

// newTestModel is a Model with just enough of an agent behind it for the event
// handlers to run: they re-subscribe to the event stream after every message.
func newTestModel(t *testing.T) *Model {
	t.Helper()
	cfg := &config.Config{
		Model:        "stub-model",
		SystemPrompt: "you are a test agent",
		Runtime:      config.RuntimeCfg{Workspace: t.TempDir()},
	}
	ag := agent.New(cfg, nil, governor.New(cfg.Model), governor.New(cfg.Model),
		nil, nil, agent.NewGatekeeper(false), nil, nil, t.TempDir())
	t.Cleanup(ag.Close)
	return &Model{ag: ag}
}

// The agent reports trouble with ErrorMsg and ends a turn with
// AssistantDoneMsg; only the second may release the UI. An error can arrive
// while a turn is still running — a second message sent by mistake is answered
// with exactly one — and ending the turn on it would hand /new, /resume and
// /reload a conversation the turn is still writing to, and leave esc pointing
// at nothing.
func TestOnlyTheDoneEventEndsATurn(t *testing.T) {
	m := newTestModel(t)
	m.busy = true
	m.cancelTurn = func() {}

	m.Update(agent.ErrorMsg{Err: "agent is still working on the previous turn"})

	if !m.busy {
		t.Error("an error ended a turn that is still running")
	}
	if m.cancelTurn == nil {
		t.Error("the running turn lost its stop key")
	}
	if !m.refuseIfBusy("new") {
		t.Error("a session switch was allowed while the turn was still running")
	}

	m.Update(agent.AssistantDoneMsg{})

	if m.busy {
		t.Error("the turn ended but the UI is still showing it as running")
	}
	if m.cancelTurn != nil {
		t.Error("the stop key still points at a turn that has ended")
	}
}

// Teardown waits for the turn to unwind before it closes the session file and
// the sandbox, so a quit that leaves the turn running is a quit the operator
// waits out.
func TestQuittingStopsTheTurnItLeavesBehind(t *testing.T) {
	m := newTestModel(t)
	stopped := false
	m.busy = true
	m.cancelTurn = func() { stopped = true }

	if cmd := m.quit(); cmd == nil {
		t.Fatal("quit returned no command, so the program would not exit")
	}
	if !stopped {
		t.Error("quitting left the turn running")
	}

	idle := newTestModel(t)
	if cmd := idle.quit(); cmd == nil {
		t.Error("quitting with no turn in flight returned no command")
	}
}
