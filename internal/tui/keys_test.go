package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/ruvolof/yaaih/internal/agent"
)

func newSizedTestModel(t *testing.T, width, height int) *Model {
	t.Helper()
	return sized(newTestModel(t), width, height)
}

// sized is a full Model around bare's agent, laid out on a width by height
// screen.
func sized(bare *Model, width, height int) *Model {
	m := New(context.Background(), nil, bare.ag, nil, "", "test", nil)
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return m
}

func press(m *Model, key tea.KeyType) {
	m.Update(tea.KeyMsg{Type: key})
}

// ctrl+h covers the middle of the screen with the shortcut list and esc takes
// it away again, leaving the screen the size it was.
func TestShortcutListOpensOverTheScreenAndClosesOnEsc(t *testing.T) {
	m := newSizedTestModel(t, 120, 40)
	before := m.View()

	press(m, tea.KeyCtrlH)
	if !m.showKeys {
		t.Fatal("ctrl+h did not open the shortcut list")
	}
	rows := strings.Split(m.View(), "\n")
	if len(rows) != m.height {
		t.Fatalf("screen is %d rows tall with the list open, want %d", len(rows), m.height)
	}
	titleRow := -1
	for index, row := range rows {
		if width := ansi.StringWidth(row); width != m.width {
			t.Errorf("row %d is %d cells wide, want %d: %q", index, width, m.width, stripANSI(row))
		}
		if strings.Contains(stripANSI(row), "Keyboard shortcuts") {
			titleRow = index
		}
	}
	if titleRow <= 0 || titleRow >= m.height/2 {
		t.Errorf("the list's title is on row %d, want it in the upper middle of %d rows", titleRow, m.height)
	}
	plain := stripANSI(m.View())
	for _, want := range []string{"^y", "alt+↑/↓", "esc to close"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the shortcut list does not mention %q", want)
		}
	}

	press(m, tea.KeyEsc)
	if m.showKeys {
		t.Fatal("esc did not close the shortcut list")
	}
	if m.View() != before {
		t.Error("the screen is not what it was before the list opened")
	}
}

// While the list is open, keys belong to it: typing must not reach the input
// box, and esc closes the list rather than stopping the running turn.
func TestShortcutListHoldsKeysBack(t *testing.T) {
	m := newSizedTestModel(t, 120, 40)
	cancelled := false
	m.busy = true
	m.cancelTurn = func() { cancelled = true }

	press(m, tea.KeyCtrlH)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	press(m, tea.KeyEnter)
	if value := m.input.Value(); value != "" {
		t.Errorf("input is %q, want the keys held back by the list", value)
	}
	press(m, tea.KeyEsc)
	if cancelled {
		t.Error("esc stopped the turn instead of closing the list")
	}
	press(m, tea.KeyEsc)
	if !cancelled {
		t.Error("esc with the list closed no longer stops the turn")
	}
}

// The list can be opened while a call waits for approval, and closing it
// leaves the request waiting rather than answering it.
func TestShortcutListOpensBesideAnApproval(t *testing.T) {
	gate := agent.NewGatekeeper(true)
	m := sized(newGatedTestModel(t, gate), 120, 40)
	go gate.Request(context.Background(), "shell_exec", "{}")
	m.Update(nextApprovalPrompt(t, m))

	press(m, tea.KeyCtrlH)
	if !m.showKeys {
		t.Fatal("ctrl+h did not open the list while a call waits for approval")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	press(m, tea.KeyEsc)
	if m.pendingApproval == nil {
		t.Error("a key pressed with the list open answered the approval")
	}
}

// On a screen too short for the whole list, the arrows scroll it to its end.
func TestShortcutListScrollsOnAShortScreen(t *testing.T) {
	m := newSizedTestModel(t, 120, 16)
	press(m, tea.KeyCtrlH)
	plain := stripANSI(m.View())
	if !strings.Contains(plain, "↑/↓ to scroll") {
		t.Fatal("the list fits; the test needs a shorter screen")
	}
	if strings.Contains(plain, "quit") {
		t.Fatal("the end of the list shows before scrolling")
	}
	for range 40 {
		press(m, tea.KeyDown)
	}
	if !strings.Contains(stripANSI(m.View()), "quit") {
		t.Error("scrolling the list down does not reach its end")
	}
	if rows := strings.Count(m.View(), "\n") + 1; rows != m.height {
		t.Errorf("screen is %d rows tall, want %d", rows, m.height)
	}
}

func TestOverlayCenterKeepsTheCellsBesideIt(t *testing.T) {
	base := strings.Join([]string{
		"0123456789",
		"\x1b[31mabcdefghij\x1b[0m",
		"ABCDEFGHIJ",
		"9876543210",
	}, "\n")
	screen := overlayCenter(base, "xx\nyy", 10, 4)
	got := strings.Split(stripANSI(screen), "\n")
	want := []string{"0123456789", "abcdxxghij", "ABCDyyGHIJ", "9876543210"}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("row %d = %q, want %q", index, got[index], want[index])
		}
	}
	coloured := strings.Split(screen, "\n")[1]
	if right := coloured[strings.Index(coloured, "xx")+2:]; !strings.Contains(right, "\x1b[31m") {
		t.Errorf("the cells right of the overlay lost their colour: %q", right)
	}
}
