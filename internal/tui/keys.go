package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// keysOverlayKey opens the shortcut list. ctrl+h is also what some terminals
// send for ctrl+backspace; backspace itself arrives as "backspace".
const keysOverlayKey = "ctrl+h"

// shortcut is one row of the shortcut list.
type shortcut struct {
	keys, effect string
}

// shortcutGroup is a titled block of the shortcut list.
type shortcutGroup struct {
	title     string
	shortcuts []shortcut
}

// shortcutGroups is the full list behind ctrl+h. The sandbox shell is listed
// only when there is a sandbox to open it in.
func (m *Model) shortcutGroups() []shortcutGroup {
	turn := []shortcut{
		{"esc", "stop the running turn"},
	}
	if m.terminal != nil {
		turn = append(turn, shortcut{"^]", "switch to a shell in the sandbox and back"})
	}
	turn = append(turn,
		shortcut{"^h", "show this list"},
		shortcut{"^c, ^d, /exit", "quit"},
	)
	return []shortcutGroup{
		{"Input", []shortcut{
			{"enter", "send"},
			{"alt+enter, ^j", "new line"},
			{"↑/↓", "walk through what you have sent"},
			{"tab", "complete commands, agents, sessions, skills"},
			{"/help", "list the slash commands"},
		}},
		{"Conversation", []shortcut{
			{"pgup/pgdn, wheel", "scroll"},
			{"^home/^end", "jump to the top / bottom"},
			{"drag", "select text"},
			{"^y", "copy the selection, or the last reply"},
			{"^t", "copy the whole conversation"},
		}},
		{"Tool calls", []shortcut{
			{"alt+↑/↓", "select an earlier / later call"},
			{"^o, click", "expand or collapse a call"},
			{"enter", "expand the selected call (empty input)"},
		}},
		{"Turn", turn},
	}
}

// keysOverlayLines lays the shortcut list out: a heading per group, then the
// keys in one column and what they do in the next.
func (m *Model) keysOverlayLines() []string {
	groups := m.shortcutGroups()
	keysWidth := 0
	for _, group := range groups {
		for _, s := range group.shortcuts {
			keysWidth = max(keysWidth, lipgloss.Width(s.keys))
		}
	}
	var lines []string
	for index, group := range groups {
		if index > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, styleHeader.Render(group.title))
		for _, s := range group.shortcuts {
			lines = append(lines, styleTool.Render(fmt.Sprintf("  %-*s", keysWidth+2, s.keys))+s.effect)
		}
	}
	return lines
}

// keysOverlayBodyHeight is how many rows of the list fit on screen inside the
// overlay's border, under its title and above its hint.
func (m *Model) keysOverlayBodyHeight() int {
	return max(m.height-styleKeysOverlay.GetVerticalFrameSize()-2, 3)
}

func (m *Model) scrollKeysOverlay(delta int) {
	maxScroll := max(len(m.keysOverlayLines())-m.keysOverlayBodyHeight(), 0)
	m.keysScroll = min(max(m.keysScroll+delta, 0), maxScroll)
}

// handleKeysOverlayKey handles a key while the shortcut list is open. Only
// quitting gets past it: esc or ctrl+h close it, the arrows scroll it, and
// everything else is held back so a key meant for the list never reaches the
// input box or the conversation behind it.
func (m *Model) handleKeysOverlayKey(key string) (passThrough bool) {
	switch key {
	case "ctrl+c", "ctrl+d":
		return true
	case "esc", "q", keysOverlayKey:
		m.showKeys = false
	case "up", "k":
		m.scrollKeysOverlay(-1)
	case "down", "j":
		m.scrollKeysOverlay(1)
	case "pgup":
		m.scrollKeysOverlay(-m.keysOverlayBodyHeight())
	case "pgdown":
		m.scrollKeysOverlay(m.keysOverlayBodyHeight())
	}
	return false
}

// handleKeysOverlayMouse scrolls the list under the wheel and ignores the rest
// of the mouse, so nothing behind the overlay is selected or scrolled.
func (m *Model) handleKeysOverlayMouse(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.scrollKeysOverlay(-3)
	case tea.MouseButtonWheelDown:
		m.scrollKeysOverlay(3)
	}
}

func (m *Model) openKeysOverlay() {
	m.showKeys = true
	m.keysScroll = 0
}

// renderKeysOverlay is the boxed shortcut list, cut to the screen's height
// with a hint when it does not fit.
func (m *Model) renderKeysOverlay() string {
	lines := m.keysOverlayLines()
	height := m.keysOverlayBodyHeight()
	first := min(m.keysScroll, max(len(lines)-height, 0))
	last := min(first+height, len(lines))
	hint := "esc to close"
	if len(lines) > height {
		hint = "↑/↓ to scroll · " + hint
	}
	body := append([]string{styleWarn.Render("Keyboard shortcuts")}, lines[first:last]...)
	body = append(body, styleMuted.Render(hint))
	maxWidth := max(m.width-styleKeysOverlay.GetHorizontalFrameSize(), 10)
	for index, line := range body {
		body[index] = ansi.Truncate(line, maxWidth, "…")
	}
	return styleKeysOverlay.Render(strings.Join(body, "\n"))
}

// overlayCenter draws overlay in the middle of base, a screen of width by
// height cells. Each covered row keeps base's cells to the left and right of
// the overlay, with their styling.
func overlayCenter(base, overlay string, width, height int) string {
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")
	overlayWidth := lipgloss.Width(overlay)
	top := max((height-len(overlayLines))/2, 0)
	left := max((width-overlayWidth)/2, 0)
	for index, overlayLine := range overlayLines {
		row := top + index
		if row >= len(baseLines) {
			break
		}
		baseLine := baseLines[row]
		before := ansi.Truncate(baseLine, left, "")
		if gap := left - ansi.StringWidth(before); gap > 0 {
			// A wide character straddling the edge is dropped, not split.
			before += strings.Repeat(" ", gap)
		}
		after := ansi.TruncateLeft(baseLine, left+overlayWidth, "")
		baseLines[row] = before + "\x1b[0m" + overlayLine + "\x1b[0m" + after
	}
	return strings.Join(baseLines, "\n")
}
