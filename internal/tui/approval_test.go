package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ruvolof/yaaih/internal/agent"
)

func TestApprovalArgsShowCommandsAsTheyRun(t *testing.T) {
	args := `{"command": "cat > /workspace/t.txt << 'EOF'\n# Title\n\nbody\nEOF", "timeout": 30, "env": {"A": "1"}}`
	fields := parseApprovalArgs(args)

	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.name)
	}
	if want := []string{"command", "timeout", "env"}; !slices.Equal(names, want) {
		t.Fatalf("field order = %q, want %q", names, want)
	}
	if want := "cat > /workspace/t.txt << 'EOF'\n# Title\n\nbody\nEOF"; fields[0].value != want {
		t.Errorf("command = %q, want %q", fields[0].value, want)
	}
	if fields[1].value != "30" {
		t.Errorf("timeout = %q, want 30", fields[1].value)
	}
	if want := "{\n  \"A\": \"1\"\n}"; fields[2].value != want {
		t.Errorf("env = %q, want %q", fields[2].value, want)
	}
}

func TestApprovalArgsThatAreNotAnObjectShowWhole(t *testing.T) {
	for _, args := range []string{"not json", `["a", "b"]`, `{"command": `} {
		fields := parseApprovalArgs(args)
		if len(fields) != 1 || fields[0].name != "" || fields[0].value != args {
			t.Errorf("parseApprovalArgs(%q) = %+v, want the arguments whole", args, fields)
		}
	}
}

func TestApprovalRevealsCharactersThatActOnTheTerminal(t *testing.T) {
	command, _ := json.Marshal("rm -rf /\rls\x1b[8m hidden\u202eabc")
	fields := parseApprovalArgs(`{"command": ` + string(command) + `}`)
	shown := stripANSI(fields[0].value)
	if want := `rm -rf /\x0dls\x1b[8m hidden\u202eabc`; shown != want {
		t.Errorf("command shown as %q, want %q", shown, want)
	}
}

func TestApprovalBodyMarksWrappedRowsApartFromLineBreaks(t *testing.T) {
	fields := []approvalField{{name: "command", value: strings.Repeat("x", 25) + "\necho done"}}
	var lines []string
	bodyLines, _ := approvalBodyLines(fields, 14)
	for _, line := range bodyLines {
		lines = append(lines, stripANSI(line))
	}
	want := []string{
		"command",
		"1 │ xxxxxxxxxx",
		"  │ xxxxxxxxxx",
		"  │ xxxxx",
		"2 │ echo done",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("body =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// approvalTestModel is a 100×30 screen with a conversation on it and a
// shell_exec call to command waiting for approval.
func approvalTestModel(t *testing.T, command string) (*Model, *agent.Gatekeeper) {
	t.Helper()
	gate := agent.NewGatekeeper(true)
	bare := newGatedTestModel(t, gate)
	m := New(context.Background(), nil, bare.ag, nil, "", "test", nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for index := range 40 {
		m.appendLine(fmt.Sprintf("narration %02d", index))
	}
	requestApproval(t, m, gate, command)
	t.Cleanup(func() {
		if m.pendingApproval != nil {
			m.pendingApproval.Cancel()
		}
	})
	return m, gate
}

func requestApproval(t *testing.T, m *Model, gate *agent.Gatekeeper, command string) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": command})
	go gate.Request(context.Background(), "shell_exec", string(args))
	m.Update(nextApprovalPrompt(t, m))
}

func longScript() string {
	return strings.Repeat("echo "+strings.Repeat("word ", 40)+"\n", 30) + "echo last-line"
}

func screenRows(m *Model) []string {
	return strings.Split(stripANSI(m.View()), "\n")
}

func rowContaining(rows []string, text string) int {
	return slices.IndexFunc(rows, func(row string) bool { return strings.Contains(row, text) })
}

func TestApprovalDocksAboveTheInputAndKeepsTheConversationInView(t *testing.T) {
	m, _ := approvalTestModel(t, "ls -la /workspace")

	rows := screenRows(m)
	if len(rows) != m.height {
		t.Fatalf("screen is %d rows tall, want %d", len(rows), m.height)
	}
	if !strings.Contains(visibleChat(m), "narration 39") {
		t.Error("the conversation leading up to the call is not in view")
	}
	title := rowContaining(rows, "Approval required")
	if want := m.approvalPanelTop() + 1; title != want {
		t.Errorf("panel title on row %d, want %d", title, want)
	}
	inputTop := m.height - (m.input.Height() + 2)
	if m.approvalPanelTop()+m.approvalHeight != inputTop {
		t.Errorf("panel ends on row %d, want it directly above the input at row %d",
			m.approvalPanelTop()+m.approvalHeight, inputTop)
	}
	if conversation := rowContaining(rows, "narration 39"); conversation < 0 || conversation >= title {
		t.Errorf("the newest narration is on row %d, want it above the panel title on row %d", conversation, title)
	}
	if !strings.Contains(stripANSI(m.View()), "ls -la /workspace") {
		t.Error("the command is not shown in the panel")
	}
}

func TestLongApprovalTakesHalfTheScreenAndScrollsToTheEnd(t *testing.T) {
	m, _ := approvalTestModel(t, longScript())

	if m.approvalHeight > m.height/2 {
		t.Errorf("panel is %d rows on a %d-row screen, want at most half", m.approvalHeight, m.height)
	}
	if width := lipgloss.Width(m.renderApprovalPanel()); width > m.width {
		t.Errorf("panel is %d columns wide on a %d-column screen", width, m.width)
	}
	screen := stripANSI(m.View())
	if strings.Contains(screen, "last-line") {
		t.Fatal("the end of the command is visible before scrolling; the test needs a longer one")
	}
	if !strings.Contains(screen, "lines 1–") {
		t.Error("an overflowing command does not say there is more to read")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(stripANSI(m.View()), "last-line") {
		t.Error("scrolling to the end does not show the last line of the command")
	}
	if m.pendingApproval == nil {
		t.Error("scrolling answered the approval")
	}
}

func TestCollapsingTheApprovalGivesItsRoomToTheConversation(t *testing.T) {
	m, gate := approvalTestModel(t, longScript())
	expandedPanel, expandedChat := m.approvalHeight, m.chat.Height

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if m.pendingApproval == nil {
		t.Fatal("collapsing answered the approval")
	}
	screen := stripANSI(m.View())
	if strings.Contains(screen, "word word") || !strings.Contains(screen, "arguments hidden") {
		t.Error("the collapsed panel still shows the command")
	}
	if !strings.Contains(screen, "[y] approve") {
		t.Error("the collapsed panel does not say how to answer it")
	}
	if given := m.chat.Height - expandedChat; given != expandedPanel-m.approvalHeight || given <= 0 {
		t.Errorf("collapsing freed %d rows but the conversation grew by %d",
			expandedPanel-m.approvalHeight, given)
	}
	if rows := screenRows(m); len(rows) != m.height {
		t.Errorf("screen is %d rows tall after collapsing, want %d", len(rows), m.height)
	}

	titleRow := m.approvalPanelTop() + styleApproval.GetBorderTopSize()
	m.Update(tea.MouseMsg{X: 5, Y: titleRow, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.approvalCollapsed || m.approvalHeight != expandedPanel || m.chat.Height != expandedChat {
		t.Error("clicking the panel title did not expand it back")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.pendingApproval != nil || m.approvalHeight != 0 {
		t.Fatal("the panel is still docked after the call was denied")
	}
	requestApproval(t, m, gate, "echo next")
	if m.approvalCollapsed {
		t.Error("a new request opened collapsed, hiding its arguments")
	}
}

func TestTheConversationStaysScrollableAndClickableWhileAnApprovalWaits(t *testing.T) {
	m, gate := approvalTestModel(t, "ls")
	m.pendingApproval.Deny()
	m.closeApproval()
	m.Update(agent.ToolCallStartMsg{ID: "call-1", Name: "shell_exec", Args: `{"command":"ls"}`})
	requestApproval(t, m, gate, longScript())
	call := lastToolEntry(m)

	clickChat(m, selectionPoint{line: call.firstLine, column: 1})
	if !call.expanded {
		t.Error("clicking a call in the conversation did not expand it")
	}

	bottom := m.chat.YOffset
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.chat.YOffset >= bottom || !m.userScrolled {
		t.Error("pgup did not scroll the conversation")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	if m.chat.YOffset != bottom || m.userScrolled {
		t.Error("ctrl+end did not return the conversation to the bottom")
	}

	chatLeft, chatTop := chatContentOrigin()
	m.Update(tea.MouseMsg{X: chatLeft + 1, Y: chatTop + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.chat.YOffset >= bottom || m.approvalScroll != 0 {
		t.Error("the wheel over the conversation did not scroll it alone")
	}
	reading := m.chat.YOffset
	m.Update(tea.MouseMsg{X: 5, Y: m.approvalPanelTop() + 3, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if m.approvalScroll == 0 || m.chat.YOffset != reading {
		t.Error("the wheel over the panel did not scroll it alone")
	}
	if m.pendingApproval == nil {
		t.Error("reading the conversation answered the approval")
	}
}

func TestTypingIsHeldBackWhileAnApprovalWaits(t *testing.T) {
	m, _ := approvalTestModel(t, "ls")
	before := m.transcript.plainText()

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if m.input.Value() != "" {
		t.Errorf("input holds %q typed while the approval waited", m.input.Value())
	}
	if m.transcript.plainText() != before {
		t.Error("enter sent something while the approval waited")
	}
	if m.pendingApproval == nil {
		t.Error("typing answered the approval")
	}
}
