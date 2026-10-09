package tui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruvolof/yaaih/internal/agent"
	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/governor"
	"github.com/ruvolof/yaaih/internal/session"
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
	if body := m.transcript.plainText(); body != "" {
		t.Fatalf("warned at 6%% full: %q", stripANSI(body))
	}

	m.noteContextFill(58_000, 64_000) // 90%
	warning := stripANSI(m.transcript.plainText())
	if !strings.Contains(warning, "90% full") || !strings.Contains(warning, "/compact") {
		t.Fatalf("warning = %q, want the fill level and what to do about it", warning)
	}

	m.noteContextFill(60_000, 64_000) // 93%, still the same filling
	if got := strings.Count(stripANSI(m.transcript.plainText()), "/compact"); got != 1 {
		t.Errorf("warned %d times during one filling, want 1", got)
	}

	// A /compact drops the fill; the next filling deserves its own warning.
	m.noteContextFill(9_000, 64_000)
	m.noteContextFill(59_000, 64_000)
	if got := strings.Count(stripANSI(m.transcript.plainText()), "/compact"); got != 2 {
		t.Errorf("warned %d times across two fillings, want 2", got)
	}
}

func TestContextWarningStaysQuietWithoutABudget(t *testing.T) {
	m := &Model{}
	m.noteContextFill(0, 0)
	m.noteContextFill(5_000, 0)

	if body := m.transcript.plainText(); body != "" {
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
	if body := m.transcript.plainText(); body != "" {
		t.Fatalf("warned at half the limit: %q", stripANSI(body))
	}

	m.noteSpend(limitedStats(10_000, 0.85, true))
	warning := stripANSI(m.transcript.plainText())
	if !strings.Contains(warning, "85%") || !strings.Contains(warning, "/new") {
		t.Fatalf("warning = %q, want how close the limit is and what to do about it", warning)
	}

	m.noteSpend(limitedStats(95_000, 0.9, true))
	if got := strings.Count(stripANSI(m.transcript.plainText()), "spending limit"); got != 1 {
		t.Errorf("warned %d times approaching one limit, want 1", got)
	}

	// /new starts from nothing; that session's approach deserves its own warning.
	m.noteSpend(limitedStats(0, 0, true))
	m.noteSpend(limitedStats(90_000, 0.1, true))
	if got := strings.Count(stripANSI(m.transcript.plainText()), "spending limit"); got != 2 {
		t.Errorf("warned %d times across two sessions, want 2", got)
	}
}

func TestUnenforceableCostLimitIsReportedOnce(t *testing.T) {
	m := &Model{}
	m.noteSpend(limitedStats(100, 0, false))
	m.noteSpend(limitedStats(200, 0, false))

	body := stripANSI(m.transcript.plainText())
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

	transcript := strings.Split(stripANSI(m.transcript.plainText()), "\n")
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

	if body := stripANSI(m.transcript.plainText()); body != "only line" {
		t.Errorf("transcript = %q, want it untouched", body)
	}
}

// newTestModel is a Model with just enough of an agent behind it for the event
// handlers to run: they re-subscribe to the event stream after every message.
func newTestModel(t *testing.T) *Model {
	t.Helper()
	return newGatedTestModel(t, agent.NewGatekeeper(false))
}

func newGatedTestModel(t *testing.T, gate *agent.Gatekeeper) *Model {
	t.Helper()
	cfg := &config.Config{
		Model:        "stub-model",
		SystemPrompt: "you are a test agent",
		Runtime:      config.RuntimeCfg{Workspace: t.TempDir()},
	}
	ag := agent.New(cfg, nil, governor.New(cfg.Model), governor.New(cfg.Model),
		nil, nil, gate, nil, session.Transcript{}, t.TempDir())
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

// nextApprovalPrompt waits for the agent to forward a prompt for the modal.
func nextApprovalPrompt(t *testing.T, m *Model) agent.ApprovalRequestMsg {
	t.Helper()
	// A model built without New has no relay yet; the agent's stream may only
	// ever have one.
	if m.events == nil {
		m.events = newEventRelay(m.ag.Events())
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-m.events.out:
			if prompt, ok := event.(agent.ApprovalRequestMsg); ok {
				return prompt
			}
		case <-timeout:
			t.Fatal("no approval prompt reached the event stream")
		}
	}
}

func TestAlwaysAllowKeysAnswerTheModal(t *testing.T) {
	cases := []struct {
		key  string
		want agent.ApprovalStanding
	}{
		{"y", agent.ApprovalStanding{Required: true}},
		{"a", agent.ApprovalStanding{Required: true, Tools: []string{"shell_exec"}}},
		{"A", agent.ApprovalStanding{Required: true, All: true}},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			gate := agent.NewGatekeeper(true)
			m := newGatedTestModel(t, gate)
			decided := make(chan agent.ApprovalDecision, 1)
			go func() {
				decision, _ := gate.Request(context.Background(), "shell_exec", "{}")
				decided <- decision
			}()

			m.Update(nextApprovalPrompt(t, m))
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})

			if m.pendingApproval != nil {
				t.Error("the modal is still open after it was answered")
			}
			select {
			case decision := <-decided:
				if decision != agent.ApprovalApprove {
					t.Errorf("decision = %v, want the call approved", decision)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the tool call is still waiting on the modal")
			}
			if standing := m.ag.ApprovalStanding(); !reflect.DeepEqual(standing, tc.want) {
				t.Errorf("standing = %+v, want %+v", standing, tc.want)
			}
		})
	}
}

func TestSidebarShowsWhatRunsWithoutAsking(t *testing.T) {
	cases := []struct {
		standing agent.ApprovalStanding
		want     []string
	}{
		{agent.ApprovalStanding{}, []string{"not required"}},
		{agent.ApprovalStanding{Required: true}, []string{"asks every call"}},
		{agent.ApprovalStanding{Required: true, Tools: []string{"file_write", "shell_exec"}},
			[]string{"auto:", "  file_write", "  shell_exec"}},
		{agent.ApprovalStanding{Required: true, All: true}, []string{"auto: all tools"}},
	}
	for _, tc := range cases {
		lines := strings.Split(strings.TrimRight(stripANSI(renderApprovalStanding(tc.standing)), "\n"), "\n")
		if want := append([]string{"Approval"}, tc.want...); !slices.Equal(lines, want) {
			t.Errorf("standing %+v renders %q, want %q", tc.standing, lines, want)
		}
	}
}

// On a terminal shorter than the sidebar, the sidebar must neither push the
// input off the screen nor hide the rest of itself out of reach.
func TestSidebarScrollsOnAShortTerminal(t *testing.T) {
	bare := newTestModel(t)
	m := New(context.Background(), nil, bare.ag, nil, "", "test", nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	for index := range 40 {
		m.appendLine(fmt.Sprintf("narration %02d", index))
	}
	screen := func() []string { return strings.Split(stripANSI(m.View()), "\n") }

	rows := screen()
	if len(rows) != m.height {
		t.Fatalf("screen is %d rows tall, want %d", len(rows), m.height)
	}
	if !strings.Contains(rows[m.mainHeight()], "more (wheel)") {
		t.Errorf("the sidebar's last row %q does not say there is more below", rows[m.mainHeight()])
	}
	if strings.Contains(strings.Join(rows, "\n"), "^c quit") {
		t.Fatal("the whole sidebar fits; the test needs a shorter screen")
	}

	sidebarX := m.chatBoxWidth() + 4
	chatBottom := m.chat.YOffset
	for range 20 {
		m.Update(tea.MouseMsg{X: sidebarX, Y: 3, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	}
	rows = screen()
	if len(rows) != m.height {
		t.Errorf("screen is %d rows tall after scrolling the sidebar, want %d", len(rows), m.height)
	}
	if !strings.Contains(strings.Join(rows, "\n"), "^c quit") {
		t.Error("scrolling the sidebar to the end does not reach its last line")
	}
	if !strings.Contains(rows[1], "more") {
		t.Errorf("the sidebar's first row %q does not say there is more above", rows[1])
	}
	if m.chat.YOffset != chatBottom || m.userScrolled {
		t.Error("the wheel over the sidebar scrolled the conversation")
	}

	m.Update(tea.MouseMsg{X: 3, Y: 3, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.chat.YOffset >= chatBottom {
		t.Error("the wheel over the conversation no longer scrolls it")
	}
	if scrolled := m.sidebarScroll; scrolled == 0 {
		t.Error("the wheel over the conversation scrolled the sidebar back")
	}
}
