package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// maxInputRows is how tall the input box grows before its content scrolls.
const maxInputRows = 8

const inputPlaceholder = "Type your message... (Enter sends, Alt+Enter or Ctrl+J adds a line, Tab completes /commands)"

// inputBox is where the operator types: a textarea that grows a row at a time
// with its content, up to maxInputRows, and scrolls past that.
type inputBox struct {
	area textarea.Model
	// measure is a second textarea at the same width that holds one line at a
	// time, to learn how many rows that line wraps to. The textarea reports
	// that only for the line under its cursor.
	measure textarea.Model
	// overflowed is whether the content was taller than maxInputRows at the
	// last fit, which leaves the textarea scrolled away from its first row.
	overflowed bool
}

func newInputBox(width int) inputBox {
	box := inputBox{area: newInputArea(), measure: newInputArea()}
	box.area.Placeholder = inputPlaceholder
	box.area.Focus()
	box.SetWidth(width)
	return box
}

// newInputArea configures a textarea for the input box. Enter is left unbound:
// it submits, and the Model handles it before the textarea sees it.
func newInputArea() textarea.Model {
	area := textarea.New()
	area.ShowLineNumbers = false
	area.CharLimit = 0
	area.MaxHeight = 0
	area.MaxWidth = 0
	area.SetPromptFunc(2, func(row int) string {
		if row == 0 {
			return "> "
		}
		return ""
	})
	area.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	area.FocusedStyle.CursorLine = lipgloss.NewStyle()
	area.FocusedStyle.Placeholder = styleMuted
	area.SetHeight(1)
	return area
}

func (box *inputBox) Value() string { return box.area.Value() }
func (box *inputBox) Height() int   { return box.area.Height() }
func (box *inputBox) View() string  { return box.area.View() }

// Update passes msg to the textarea and reports whether the box changed height.
func (box *inputBox) Update(msg tea.Msg) (cmd tea.Cmd, resized bool) {
	box.area, cmd = box.area.Update(msg)
	return cmd, box.fit()
}

// SetValue replaces the content, leaving the cursor at its end, and reports
// whether the box changed height.
func (box *inputBox) SetValue(text string) (resized bool) {
	box.area.SetValue(text)
	return box.fit()
}

// SetWidth sets the width, prompt included, and reports whether rewrapping the
// content at it changed the box's height.
func (box *inputBox) SetWidth(width int) (resized bool) {
	box.area.SetWidth(width)
	box.measure.SetWidth(width)
	return box.fit()
}

func (box *inputBox) fit() (resized bool) {
	contentRows := box.contentRows(maxInputRows + 1)
	rows := min(contentRows, maxInputRows)
	resized = rows != box.area.Height()
	if resized {
		box.area.SetHeight(rows)
	}
	fits := contentRows <= maxInputRows
	if fits && (resized || box.overflowed) {
		box.scrollToTop()
	}
	box.overflowed = !fits
	return resized
}

// contentRows counts the rows the content wraps to, stopping at limit.
func (box *inputBox) contentRows(limit int) int {
	rows := 0
	for _, line := range strings.Split(box.area.Value(), "\n") {
		box.measure.SetValue(line)
		rows += box.measure.LineInfo().Height
		if rows >= limit {
			return limit
		}
	}
	return rows
}

// scrollToTop brings the textarea's first row back into view, keeping the
// cursor where it was. The textarea scrolls to follow the cursor but never
// scrolls back when it is made taller, so a box that grew as the cursor went
// down would otherwise keep its first rows hidden with room to show them.
// SetValue is the only way to reset that scroll from outside the textarea.
func (box *inputBox) scrollToTop() {
	info := box.area.LineInfo()
	row, column := box.area.Line(), info.StartColumn+info.ColumnOffset
	box.area.SetValue(box.area.Value())
	for box.area.Line() > row {
		box.area.CursorUp()
	}
	box.area.SetCursor(column)
}
