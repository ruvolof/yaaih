package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// thumbRows are the screen rows the chat's thumb is drawn on.
func thumbRows(m *Model) []int {
	borderX := styleChatBorder.GetBorderLeftSize() + m.chatBoxWidth()
	var rows []int
	for index, row := range screenRows(m) {
		if ansi.Cut(row, borderX, borderX+1) == "█" {
			rows = append(rows, index)
		}
	}
	return rows
}

func scrollbarPress(m *Model, y int) {
	x := styleChatBorder.GetBorderLeftSize() + m.chatBoxWidth()
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
}

func scrollbarMotion(m *Model, y int) {
	x := styleChatBorder.GetBorderLeftSize() + m.chatBoxWidth()
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
}

func scrollbarRelease(m *Model, y int) {
	x := styleChatBorder.GetBorderLeftSize() + m.chatBoxWidth()
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonNone})
}

func TestScrollbarAppearsOnlyWhenTheChatOverflows(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine("a short conversation")
	if rows := thumbRows(m); len(rows) != 0 {
		t.Fatalf("a conversation that fits draws a thumb on rows %v", rows)
	}

	m.appendLine(numberedLines(200))
	rows := thumbRows(m)
	if len(rows) == 0 {
		t.Fatal("an overflowing conversation draws no thumb")
	}
	_, top := chatContentOrigin()
	if last := rows[len(rows)-1]; last != top+m.chat.Height-1 {
		t.Errorf("following the bottom, the thumb ends on row %d, want the viewport's last row %d", last, top+m.chat.Height-1)
	}
	if strings.Contains(strings.Join(screenRows(m), "\n"), "at bottom") {
		t.Error("the sidebar still reports the scroll position")
	}
}

func TestDraggingTheThumbScrollsTheChat(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine(numberedLines(200))
	_, top := chatContentOrigin()
	thumb, _ := m.chatThumb()
	grabbed := top + thumb.top
	bottom := m.chat.YOffset

	scrollbarPress(m, grabbed)
	if m.chat.YOffset != bottom {
		t.Fatal("pressing on the thumb moved it")
	}
	scrollbarMotion(m, grabbed-thumb.top)
	if m.chat.YOffset != 0 || !m.userScrolled {
		t.Errorf("dragging the thumb to the top leaves the chat at line %d", m.chat.YOffset)
	}
	if m.selection != nil {
		t.Error("dragging the thumb selected text")
	}
	scrollbarMotion(m, top+m.chat.Height+5)
	scrollbarRelease(m, top+m.chat.Height+5)
	if !m.chat.AtBottom() || m.userScrolled {
		t.Error("dragging the thumb back down does not return to following the bottom")
	}

	m.appendLine("arrives after the drag")
	if !m.chat.AtBottom() {
		t.Error("new content does not follow the bottom after the drag ends there")
	}
}

func TestPressingTheTrackJumpsThere(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine(numberedLines(200))
	_, top := chatContentOrigin()

	scrollbarPress(m, top)
	scrollbarRelease(m, top)
	if m.chat.YOffset != 0 {
		t.Errorf("pressing the top of the track leaves the chat at line %d", m.chat.YOffset)
	}
	if m.draggingThumb {
		t.Error("the drag outlives its release")
	}

	scrollbarPress(m, top+m.chat.Height/2)
	thumb, _ := m.chatThumb()
	if !thumb.contains(m.chat.Height / 2) {
		t.Errorf("pressing the middle of the track puts the thumb at rows %d–%d", thumb.top, thumb.top+thumb.size-1)
	}
}
