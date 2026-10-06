package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruvolof/yaaih/internal/agent"
	"github.com/ruvolof/yaaih/internal/llm"
)

// sizedTestModel is a Model laid out on a 120×40 screen, so clicks and
// scrolling have a viewport to land in.
func sizedTestModel(t *testing.T) *Model {
	t.Helper()
	bare := newTestModel(t)
	m := New(context.Background(), nil, bare.ag, nil, "", "test", nil)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

func numberedLines(count int) string {
	var lines []string
	for i := range count {
		lines = append(lines, fmt.Sprintf("output line %03d", i))
	}
	return strings.Join(lines, "\n")
}

func runTool(m *Model, id, args, result string) {
	m.Update(agent.ToolCallStartMsg{ID: id, Name: "shell_exec", Args: args})
	m.Update(agent.ToolCallResultMsg{ID: id, Name: "shell_exec", Result: result})
}

func visibleChat(m *Model) string {
	return stripANSI(m.chat.View())
}

func lastToolEntry(m *Model) *chatEntry {
	return m.transcript.toolBefore(len(m.transcript.entries))
}

// The reply is drawn under the call it answers even when something else was
// shown while the call ran — a timeout warning arrives between the two.
func TestToolReplyLandsUnderItsCall(t *testing.T) {
	m := sizedTestModel(t)
	m.Update(agent.ToolCallStartMsg{ID: "call-1", Name: "shell_exec", Args: `{"command":"sleep 99"}`})
	m.Update(agent.TimeoutMsg{Command: "sleep 99", Message: "timed out"})
	m.Update(agent.ToolCallResultMsg{ID: "call-1", Name: "shell_exec", Result: "error: timed out", Err: "timed out"})

	transcript := m.transcript.plainText()
	call := strings.Index(transcript, "shell_exec")
	reply := strings.Index(transcript, "✗")
	warning := strings.Index(transcript, "Command timed out")
	if call < 0 || reply < 0 || warning < 0 || !(call < reply && reply < warning) {
		t.Errorf("want call, reply, then warning; transcript:\n%s", transcript)
	}
	if entries := len(m.transcript.entries); entries != 2 {
		t.Errorf("entries = %d, want the call and the warning", entries)
	}
}

// What the operator can expand to is the reply the model was given, every line
// of it; collapsed, only a preview shows.
func TestExpandedToolShowsTheWholeReply(t *testing.T) {
	m := sizedTestModel(t)
	reply := numberedLines(60)
	runTool(m, "call-1", `{"command":"seq 60"}`, reply)

	if strings.Contains(visibleChat(m), "output line 059") {
		t.Fatal("the collapsed call already shows the end of a long reply")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})

	entry := lastToolEntry(m)
	if !entry.expanded {
		t.Fatal("ctrl+o did not expand the newest call")
	}
	for _, line := range strings.Split(reply, "\n") {
		if !strings.Contains(entry.layout, line) {
			t.Fatalf("expanded call is missing %q", line)
		}
	}
	if !strings.Contains(visibleChat(m), "▾ shell_exec") {
		t.Errorf("expanding scrolled the call's heading out of view:\n%s", visibleChat(m))
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if entry.expanded {
		t.Error("a second ctrl+o did not collapse the call")
	}
}

// Wrapped reply lines must not reflow: in output like a table, a space is
// content.
func TestExpandedReplyKeepsItsSpacing(t *testing.T) {
	m := sizedTestModel(t)
	runTool(m, "call-1", `{"command":"ps"}`, "PID   CMD\n1     init")
	m.toggleTool(lastToolEntry(m))

	if !strings.Contains(stripANSI(lastToolEntry(m).layout), toolResultIndent+"PID   CMD") {
		t.Errorf("reply lost its spacing:\n%s", stripANSI(lastToolEntry(m).layout))
	}
}

func TestClickingACallTogglesIt(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine("narration above the call")
	runTool(m, "call-1", `{"command":"ls"}`, "a\nb")
	entry := lastToolEntry(m)

	click := func(line int) { clickChat(m, selectionPoint{line: line, column: 1}) }

	click(m.transcript.entries[0].firstLine)
	if entry.expanded {
		t.Fatal("clicking narration expanded a call")
	}
	click(entry.firstLine + entry.lineCount - 1)
	if !entry.expanded {
		t.Fatal("clicking the call's last line did not expand it")
	}
	click(entry.firstLine)
	if entry.expanded {
		t.Error("clicking the expanded call's heading did not collapse it")
	}
}

func TestAltArrowsSelectCallsAndEnterTogglesTheSelection(t *testing.T) {
	m := sizedTestModel(t)
	runTool(m, "call-1", `{"command":"one"}`, "1")
	m.appendLine("between")
	runTool(m, "call-2", `{"command":"two"}`, "2")
	first, second := m.transcript.entries[0], m.transcript.entries[2]

	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if m.transcript.selected != second {
		t.Fatal("alt+↑ with nothing selected did not select the newest call")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if m.transcript.selected != first {
		t.Fatal("alt+↑ did not stop at the oldest call")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !first.expanded || second.expanded {
		t.Fatal("enter on an empty input did not toggle the selected call")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if first.expanded {
		t.Fatal("ctrl+o did not act on the selected call")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	if m.transcript.selected != nil {
		t.Error("alt+↓ past the newest call left a selection behind")
	}
}

// A selection must not swallow a message being sent.
func TestEnterWithTextSendsDespiteASelection(t *testing.T) {
	m := sizedTestModel(t)
	runTool(m, "call-1", `{"command":"one"}`, "1")
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	m.setInput("/help")

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if lastToolEntry(m).expanded {
		t.Error("enter toggled the call instead of running the command")
	}
	if !strings.Contains(m.transcript.plainText(), "» /help") {
		t.Error("the command was not run")
	}
}

// A resumed session shows its calls the way the live loop did, failures
// included.
func TestResumedToolCallsCanBeExpanded(t *testing.T) {
	m := sizedTestModel(t)
	m.prefillFromHistory([]llm.Message{
		{Role: llm.RoleUser, Content: "list it"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "shell_exec", Arguments: `{"command":"ls"}`},
			{ID: "call-2", Name: "shell_exec", Arguments: `{"command":"nope"}`},
		}},
		{Role: llm.RoleTool, ToolCallID: "call-1", Name: "shell_exec", Content: "a\nb"},
		{Role: llm.RoleTool, ToolCallID: "call-2", Name: "shell_exec", Content: "error: exit status 127\nnope: not found"},
	})

	var calls []*toolCall
	for _, entry := range m.transcript.entries {
		if entry.tool != nil {
			calls = append(calls, entry.tool)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].failed || calls[0].result != "a\nb" {
		t.Errorf("first call = %+v, want its reply and no failure", calls[0])
	}
	if !calls[1].failed || !strings.Contains(calls[1].result, "not found") {
		t.Errorf("second call = %+v, want it marked failed with its reply", calls[1])
	}
}

// The copied transcript is for reading elsewhere, so it carries every reply
// whole whatever is expanded on screen.
func TestCopiedTranscriptHasEveryReplyInFull(t *testing.T) {
	m := sizedTestModel(t)
	reply := numberedLines(60)
	runTool(m, "call-1", `{"command":"seq 60"}`, reply)

	if !strings.Contains(m.transcript.plainText(), "output line 059") {
		t.Error("the plain transcript truncated a collapsed reply")
	}
}
