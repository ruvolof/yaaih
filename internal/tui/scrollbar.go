package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The chat's scrollbar is drawn on the chat box's right border, the border
// itself being the track, alongside the rows of the viewport. Its thumb can be
// dragged, and a press on the track brings the thumb's middle to the pointer.

// scrollThumb is where the thumb is: size rows, starting top rows below the
// first row of the viewport.
type scrollThumb struct {
	top, size int
}

func (thumb scrollThumb) contains(row int) bool {
	return row >= thumb.top && row < thumb.top+thumb.size
}

// chatThumb is the chat's thumb for the current scroll position, or false when
// the whole conversation fits and there is nothing to scroll.
func (m *Model) chatThumb() (scrollThumb, bool) {
	track, total := m.chat.Height, m.chat.TotalLineCount()
	if track <= 0 || total <= track {
		return scrollThumb{}, false
	}
	size := max(track*track/total, 1)
	travel := track - size
	maxOffset := total - track
	top := (m.chat.YOffset*travel + maxOffset/2) / maxOffset
	return scrollThumb{top: min(max(top, 0), travel), size: size}, true
}

// overChatScrollbar reports whether screen cell (x, y) is on the scrollbar. The
// padding column beside the border counts too, so the thumb is not a
// one-cell-wide target.
func (m *Model) overChatScrollbar(x, y int) bool {
	_, top := chatContentOrigin()
	borderX := styleChatBorder.GetBorderLeftSize() + m.chatBoxWidth()
	return x >= borderX-styleChatBorder.GetPaddingRight() && x <= borderX &&
		y >= top && y < top+m.chat.Height
}

// grabScrollbar starts a drag of the thumb when screen cell (x, y) is on the
// scrollbar, reporting whether it was. A press on the thumb holds it at the row
// it was grabbed by and does not move it; one on the track moves the thumb's
// middle to the pointer.
func (m *Model) grabScrollbar(x, y int) bool {
	if !m.overChatScrollbar(x, y) {
		return false
	}
	thumb, ok := m.chatThumb()
	if !ok {
		return false
	}
	_, top := chatContentOrigin()
	m.draggingThumb = true
	if row := y - top; thumb.contains(row) {
		m.thumbGrab = row - thumb.top
		return true
	}
	m.thumbGrab = thumb.size / 2
	m.dragThumbTo(y)
	return true
}

// dragScrollbar carries a drag of the thumb through to its release, reporting
// whether msg belonged to one.
func (m *Model) dragScrollbar(msg tea.MouseMsg) bool {
	if !m.draggingThumb {
		return false
	}
	switch msg.Action {
	case tea.MouseActionMotion:
		m.dragThumbTo(msg.Y)
	case tea.MouseActionRelease:
		m.draggingThumb = false
	default:
		return false
	}
	return true
}

// dragThumbTo scrolls the chat so the thumb's grabbed row is at screen row y,
// as near as the track allows. Dragging it to the bottom follows new content
// again, as scrolling there any other way does.
func (m *Model) dragThumbTo(y int) {
	thumb, ok := m.chatThumb()
	if !ok {
		return
	}
	_, top := chatContentOrigin()
	travel := m.chat.Height - thumb.size
	if travel <= 0 {
		return
	}
	thumbTop := min(max(y-top-m.thumbGrab, 0), travel)
	maxOffset := m.chat.TotalLineCount() - m.chat.Height
	m.chat.SetYOffset((thumbTop*maxOffset + travel/2) / travel)
	m.userScrolled = !m.chat.AtBottom()
}

// drawChatScrollbar draws the thumb over the right border of box, the chat box
// as rendered.
func (m *Model) drawChatScrollbar(box string) string {
	thumb, ok := m.chatThumb()
	if !ok {
		return box
	}
	_, top := chatContentOrigin()
	rows := strings.Split(box, "\n")
	for row := top + thumb.top; row < min(top+thumb.top+thumb.size, len(rows)); row++ {
		width := ansi.StringWidth(rows[row])
		rows[row] = ansi.Cut(rows[row], 0, width-1) + ansi.ResetStyle + styleScrollThumb.Render("█")
	}
	return strings.Join(rows, "\n")
}
