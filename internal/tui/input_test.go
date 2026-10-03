package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/governor"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
)

// newInputTestModel is an agent-less Model laid out on a 100x40 screen, which
// is all the input box needs.
func newInputTestModel(t *testing.T) *Model {
	t.Helper()
	model := New(context.Background(), nil, nil, nil, t.TempDir(), "", nil)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return model
}

// sendKey delivers a key the way a running program does: Update, then View.
// The textarea learns how far it can scroll only when it is drawn.
func sendKey(model *Model, key tea.KeyMsg) {
	model.Update(key)
	model.View()
}

func typeText(model *Model, text string) {
	sendKey(model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

func pressKey(model *Model, keyType tea.KeyType) {
	sendKey(model, tea.KeyMsg{Type: keyType})
}

func TestInputHistoryWalksBackAndForth(t *testing.T) {
	history := NewInputHistory()
	for _, text := range []string{"first", "second", "third"} {
		history.record(text)
	}

	var walked []string
	current := ""
	for {
		entry, ok := history.previous(current)
		if !ok {
			break
		}
		walked = append(walked, entry)
		current = entry
	}
	if want := []string{"third", "second", "first"}; !slices.Equal(walked, want) {
		t.Fatalf("↑ walked %q, want %q", walked, want)
	}

	walked = nil
	for {
		entry, ok := history.next(current)
		if !ok {
			break
		}
		walked = append(walked, entry)
		current = entry
	}
	if want := []string{"second", "third", ""}; !slices.Equal(walked, want) {
		t.Errorf("↓ walked %q, want %q, ending on an empty input", walked, want)
	}
}

// Text the operator typed is theirs: ↑ must not throw it away for an old entry.
func TestInputHistoryLeavesEditedInputAlone(t *testing.T) {
	history := NewInputHistory()
	history.record("first")
	history.record("second")

	if _, ok := history.previous("half-typed"); ok {
		t.Error("↑ replaced text the operator typed")
	}
	recalled, _ := history.previous("")
	if _, ok := history.previous(recalled + " edited"); ok {
		t.Error("↑ replaced a recalled entry the operator had edited")
	}
	if _, ok := history.next(recalled + " edited"); ok {
		t.Error("↓ replaced a recalled entry the operator had edited")
	}
}

func TestInputHistorySkipsRepeatsAndForgetsTheOldest(t *testing.T) {
	history := NewInputHistory()
	history.record("same")
	history.record("same")
	history.record("")
	if !slices.Equal(history.entries, []string{"same"}) {
		t.Errorf("entries = %q, want one entry for a repeat and none for an empty input", history.entries)
	}

	for i := range inputHistoryLimit + 5 {
		history.record(fmt.Sprintf("entry %d", i))
	}
	if len(history.entries) != inputHistoryLimit {
		t.Fatalf("kept %d entries, want %d", len(history.entries), inputHistoryLimit)
	}
	if history.entries[0] != "entry 5" {
		t.Errorf("oldest kept entry = %q, want the five oldest forgotten", history.entries[0])
	}
}

func TestUpArrowRecallsWhatWasSubmitted(t *testing.T) {
	model := newInputTestModel(t)
	typeText(model, "/agents")
	pressKey(model, tea.KeyEnter)
	if got := model.input.Value(); got != "" {
		t.Fatalf("input after Enter = %q, want it cleared", got)
	}

	pressKey(model, tea.KeyUp)
	if got := model.input.Value(); got != "/agents" {
		t.Errorf("input after ↑ = %q, want the submitted command back", got)
	}
	pressKey(model, tea.KeyDown)
	if got := model.input.Value(); got != "" {
		t.Errorf("input after ↓ = %q, want it empty again", got)
	}
}

// Switching agents builds a new Model; what was typed to the last one must
// still be a ↑ away.
func TestInputHistoryCarriesAcrossModels(t *testing.T) {
	history := NewInputHistory()
	first := New(context.Background(), nil, nil, nil, t.TempDir(), "", history)
	typeText(first, "/agents")
	pressKey(first, tea.KeyEnter)

	second := New(context.Background(), nil, nil, nil, t.TempDir(), "", history)
	pressKey(second, tea.KeyUp)
	if got := second.input.Value(); got != "/agents" {
		t.Errorf("↑ in the next Model = %q, want the input sent before the switch", got)
	}
}

func TestEnterSendsAndAltEnterBreaksTheLine(t *testing.T) {
	model := newInputTestModel(t)
	typeText(model, "line one")
	sendKey(model, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	typeText(model, "line two")
	pressKey(model, tea.KeyCtrlJ)
	typeText(model, "line three")

	if got, want := model.input.Value(), "line one\nline two\nline three"; got != want {
		t.Fatalf("input = %q, want %q", got, want)
	}
	if got := model.input.Height(); got != 3 {
		t.Errorf("input box is %d rows for three lines, want 3", got)
	}

	pressKey(model, tea.KeyEnter)
	if got := model.input.Value(); got != "" {
		t.Errorf("input after Enter = %q, want it submitted and cleared", got)
	}
	if got := model.input.Height(); got != 1 {
		t.Errorf("input box is %d rows after sending, want it back to 1", got)
	}
}

// The box grows to show what is typed, the first rows included, and the chat
// gives up the room it takes.
func TestInputBoxGrowsUpToItsLimitAndShowsItsFirstRow(t *testing.T) {
	model := newInputTestModel(t)
	chatHeight := model.chat.Height

	typeText(model, "first line")
	for i := 2; i <= 4; i++ {
		pressKey(model, tea.KeyCtrlJ)
		typeText(model, fmt.Sprintf("line %d", i))
	}
	if got := model.input.Height(); got != 4 {
		t.Fatalf("input box is %d rows for four lines, want 4", got)
	}
	if got := model.chat.Height; got != chatHeight-3 {
		t.Errorf("chat height = %d, want %d: three rows given to the input box", got, chatHeight-3)
	}
	if view := stripANSI(model.input.View()); !strings.Contains(view, "first line") {
		t.Errorf("the grown input box hides its first row:\n%s", view)
	}

	for i := 5; i <= maxInputRows+3; i++ {
		pressKey(model, tea.KeyCtrlJ)
		typeText(model, fmt.Sprintf("line %d", i))
	}
	if got := model.input.Height(); got != maxInputRows {
		t.Errorf("input box is %d rows, want it capped at %d", got, maxInputRows)
	}
	if view := stripANSI(model.input.View()); !strings.Contains(view, fmt.Sprintf("line %d", maxInputRows+3)) {
		t.Errorf("the overflowing input box does not follow the cursor:\n%s", view)
	}
}

func TestInputBoxGrowsForALineThatWraps(t *testing.T) {
	model := newInputTestModel(t)
	typeText(model, strings.Repeat("word ", 50))

	if got := model.input.Height(); got < 2 {
		t.Errorf("input box is %d rows for a line wider than it, want it to grow", got)
	}
	if view := stripANSI(model.input.View()); !strings.HasPrefix(strings.TrimSpace(view), "> word") {
		t.Errorf("the wrapped line's first row is not shown:\n%s", view)
	}
}

func TestSplitCommandSeparatesOnANewline(t *testing.T) {
	name, args := splitCommand("/compact\nkeep the plan")
	if name != "compact" || args != "keep the plan" {
		t.Errorf("splitCommand = (%q, %q), want (compact, \"keep the plan\")", name, args)
	}
}

func TestTabCompletesCommandNames(t *testing.T) {
	model := &Model{}
	cases := []struct {
		text, completed string
		candidates      []string
	}{
		{"/res", "/resume ", nil},
		{"/ski", "/skill", nil},
		{"/skill", "/skill", []string{"skill", "skills"}},
		{"/qu", "/quit ", nil},
		{"/zzz", "/zzz", nil},
		{"plain text", "plain text", nil},
	}
	for _, tc := range cases {
		completed, candidates := model.completeInput(tc.text)
		if completed != tc.completed || !slices.Equal(candidates, tc.candidates) {
			t.Errorf("completeInput(%q) = (%q, %q), want (%q, %q)",
				tc.text, completed, candidates, tc.completed, tc.candidates)
		}
	}
}

// writeAgentFolder creates an agent under agentsDir whose sessions live in
// its own sessions/ folder, holding the given session ids.
func writeAgentFolder(t *testing.T, agentsDir, name string, sessionIDs ...string) string {
	t.Helper()
	folder := filepath.Join(agentsDir, name)
	sessionsDir := filepath.Join(folder, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentFile := "---\nname: " + name + "\nmodel: test-model\nprovider:\n  base_url: https://example.com/v1\n---\nYou are a test agent.\n"
	if err := os.WriteFile(filepath.Join(folder, "agent.md"), []byte(agentFile), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, id := range sessionIDs {
		if err := os.WriteFile(filepath.Join(sessionsDir, id+".jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return sessionsDir
}

// /resume takes any agent's session, so Tab must offer them all — and the
// operator must be able to get from a timestamp prefix to one id.
func TestTabCompletesSessionsAcrossAgents(t *testing.T) {
	agentsDir := t.TempDir()
	alphaSessions := writeAgentFolder(t, agentsDir, "alpha", "20261001-090000-aaaa", "20261002-090000-bbbb")
	writeAgentFolder(t, agentsDir, "beta", "20261003-090000-cccc", "deploy-notes")

	cfg := &config.Config{Model: "stub-model", Runtime: config.RuntimeCfg{Workspace: t.TempDir()}}
	ag := agent.New(cfg, nil, governor.New(cfg.Model), governor.New(cfg.Model),
		nil, nil, agent.NewGatekeeper(false), nil, session.Transcript{}, alphaSessions)
	t.Cleanup(ag.Close)
	model := New(context.Background(), cfg, ag, nil, agentsDir, "alpha", nil)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	_, candidates := model.completeInput("/resume ")
	if want := []string{"deploy-notes", "20261003-090000-cccc", "20261002-090000-bbbb", "20261001-090000-aaaa"}; !slices.Equal(candidates, want) {
		t.Errorf("candidates = %q, want every agent's sessions, newest first", candidates)
	}

	typeText(model, "/resume 2026100")
	pressKey(model, tea.KeyTab)
	if got := model.input.Value(); got != "/resume 2026100" {
		t.Fatalf("input = %q, want it left alone with three sessions matching", got)
	}
	if listed := stripANSI(model.chatBuf.String()); !strings.Contains(listed, "20261003-090000-cccc") {
		t.Errorf("Tab did not list the matching sessions: %q", listed)
	}

	typeText(model, "3")
	pressKey(model, tea.KeyTab)
	if got := model.input.Value(); got != "/resume 20261003-090000-cccc" {
		t.Errorf("input = %q, want the one matching session filled in", got)
	}
}

func TestTabCompletesAgentsAndSkills(t *testing.T) {
	agentsDir := t.TempDir()
	writeAgentFolder(t, agentsDir, "chatter")
	writeAgentFolder(t, agentsDir, "devops")
	model := newSkilledTestModel(t)
	model.agentsDir = agentsDir

	if completed, _ := model.completeInput("/new dev"); completed != "/new devops" {
		t.Errorf("/new dev completes to %q, want /new devops", completed)
	}
	if completed, _ := model.completeInput("/skill n"); completed != "/skill network" {
		t.Errorf("/skill n completes to %q, want /skill network", completed)
	}
	if completed, candidates := model.completeInput("/skill s"); completed != "/skill s" || candidates != nil {
		t.Errorf("/skill s = (%q, %q), want nothing: the only s-skill is always loaded", completed, candidates)
	}
	if completed, _ := model.completeInput("/sessions a"); completed != "/sessions all" {
		t.Errorf("/sessions a completes to %q, want /sessions all", completed)
	}
}

func TestCompletionListIsCapped(t *testing.T) {
	candidates := make([]string, maxListedCompletions+7)
	for i := range candidates {
		candidates[i] = fmt.Sprintf("s%02d", i)
	}
	listed := stripANSI(renderCompletions(candidates))
	if !strings.HasSuffix(listed, "… and 7 more") || strings.Contains(listed, candidates[maxListedCompletions]) {
		t.Errorf("listing = %q, want %d candidates and a count of the rest", listed, maxListedCompletions)
	}
}
