package tui

import (
	"context"
	"encoding/json"
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
	for _, line := range approvalBodyLines(fields, 14) {
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

func TestLongApprovalFitsTheScreenAndScrollsToTheEnd(t *testing.T) {
	gate := agent.NewGatekeeper(true)
	m := newGatedTestModel(t, gate)
	m.width, m.height = 100, 30
	script := strings.Repeat("echo "+strings.Repeat("word ", 40)+"\n", 30) + "echo last-line"
	command, _ := json.Marshal(script)
	go gate.Request(context.Background(), "shell_exec", `{"command": `+string(command)+`}`)
	m.Update(nextApprovalPrompt(t, m))
	t.Cleanup(func() { m.pendingApproval.Cancel() })

	modal := styleApproval.Render(m.renderApproval())
	if width := lipgloss.Width(modal); width > m.width {
		t.Errorf("modal is %d columns wide on a %d-column screen", width, m.width)
	}
	if height := lipgloss.Height(modal); height > m.height {
		t.Errorf("modal is %d rows tall on a %d-row screen", height, m.height)
	}
	if strings.Contains(modal, "last-line") {
		t.Fatal("the end of the command is visible before scrolling; the test needs a longer one")
	}
	if !strings.Contains(stripANSI(modal), "lines 1–") {
		t.Error("an overflowing command does not say there is more to read")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(m.renderApproval(), "last-line") {
		t.Error("scrolling to the end does not show the last line of the command")
	}
	if m.pendingApproval == nil {
		t.Error("scrolling answered the approval")
	}
}
