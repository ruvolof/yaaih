package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// selectionPane is a part of the screen whose text can be selected with the
// mouse. A selection stays within the pane it started in.
type selectionPane int

const (
	chatPane selectionPane = iota
	inputPane
	approvalPane
)

// selectionPoint is a place in a pane's laid-out text: a line of it and a
// display cell along that line. It is not a screen position, so a selection
// stays on the same text while the pane scrolls.
type selectionPoint struct {
	line   int
	column int
}

func (point selectionPoint) before(other selectionPoint) bool {
	return point.line < other.line || (point.line == other.line && point.column < other.column)
}

// textSelection is the text the operator has dragged over in one pane, from
// anchor, where the drag started, to head, where the pointer is now. moved
// tells a drag from a click: a press and release on the same cell selects
// nothing.
type textSelection struct {
	pane   selectionPane
	anchor selectionPoint
	head   selectionPoint
	moved  bool
}

// span is the selection in reading order, end exclusive: the cell under the
// pointer at either end is part of it.
func (selection *textSelection) span() (start, end selectionPoint) {
	start, end = selection.anchor, selection.head
	if end.before(start) {
		start, end = end, start
	}
	end.column++
	return start, end
}

// reaches reports whether the selection covers any line at or after line.
func (selection *textSelection) reaches(line int) bool {
	return max(selection.anchor.line, selection.head.line) >= line
}

// lineInfo is what copying needs to know about a laid-out line that its text
// does not say. continues is set when the line only carries on the one above,
// broken there to fit the width, and joiner is what the break took out: the
// space a word wrap drops, or nothing where a line was cut mid-word. gutter is
// how many leading cells are drawn beside the text rather than being part of
// it — an indent, or a line number.
type lineInfo struct {
	continues bool
	joiner    string
	gutter    int
}

// infoAt is info's entry for line index, or the zero lineInfo — an unbroken
// line with no gutter — when info does not reach it.
func infoAt(info []lineInfo, index int) lineInfo {
	if index < len(info) {
		return info[index]
	}
	return lineInfo{}
}

// wrapEach lays each line of text out with wrap and records, for every line
// that produces, whether it only carries on the one above. info is nil when no
// line was broken.
func wrapEach(text string, wrap func(string) string) (lines []string, info []lineInfo) {
	for _, source := range strings.Split(text, "\n") {
		pieces := strings.Split(wrap(source), "\n")
		switch {
		case len(pieces) > 1:
			if info == nil {
				info = make([]lineInfo, len(lines), len(lines)+len(pieces))
			}
			info = append(info, breaksIn(source, pieces)...)
		case info != nil:
			info = append(info, lineInfo{})
		}
		lines = append(lines, pieces...)
	}
	return lines, info
}

// breaksIn finds what wrapping source into pieces took out at each break, by
// looking for every piece's text in source after the one before it.
func breaksIn(source string, pieces []string) []lineInfo {
	info := make([]lineInfo, len(pieces))
	plain := ansi.Strip(source)
	position := 0
	for index, piece := range pieces {
		text := ansi.Strip(piece)
		found := strings.Index(plain[position:], text)
		if index > 0 {
			info[index].continues = true
			if found > 0 {
				info[index].joiner = joinerFor(plain[position : position+found])
			}
		}
		if found >= 0 {
			position += found + len(text)
		}
	}
	return info
}

// joinerFor keeps the text a break took out without holding on to the whole
// line it was cut from.
func joinerFor(gap string) string {
	if gap == " " {
		return " "
	}
	return strings.Clone(gap)
}

// growInfo makes info cover count lines, the ones it did not reach being
// unbroken and without a gutter.
func growInfo(info []lineInfo, count int) []lineInfo {
	for len(info) < count {
		info = append(info, lineInfo{})
	}
	return info
}

// selectedText is the text between start and end as it reads on screen:
// without styling, without gutters, and with each line that was broken to fit
// the width joined back onto the one before it. lines, with info alongside,
// are the laid-out lines from firstLine on.
func selectedText(lines []string, info []lineInfo, firstLine int, start, end selectionPoint) string {
	var logicalLines []string
	var current strings.Builder
	for index, line := range lines {
		number := firstLine + index
		if number < start.line || number > end.line {
			continue
		}
		lineInfo := infoAt(info, index)
		if number > start.line {
			if lineInfo.continues {
				current.WriteString(lineInfo.joiner)
			} else {
				logicalLines = append(logicalLines, strings.TrimRight(current.String(), " "))
				current.Reset()
			}
		}
		plain := ansi.Strip(line)
		left, right := lineInfo.gutter, ansi.StringWidth(plain)
		if number == start.line {
			left = max(left, start.column)
		}
		if number == end.line {
			right = min(right, end.column)
		}
		current.WriteString(withoutClusterBreaks(ansi.Cut(plain, left, right)))
	}
	logicalLines = append(logicalLines, strings.TrimRight(current.String(), " "))
	return strings.Join(logicalLines, "\n")
}

// highlightRows draws the part of rows between start and end in the selection
// colour, leaving gutters alone. rows are the lines on screen, the first of
// them being firstLine of the laid-out text, with info alongside. The
// highlighted cells lose their own styling, the way a terminal's selection
// covers what is under it; the cells after them keep theirs, because cutting a
// line carries its escape sequences along.
func highlightRows(rows []string, info []lineInfo, firstLine int, start, end selectionPoint) {
	for index, row := range rows {
		number := firstLine + index
		if number < start.line || number > end.line {
			continue
		}
		width := ansi.StringWidth(row)
		left, right := min(infoAt(info, index).gutter, width), width
		if number == start.line {
			left = max(left, min(start.column, width))
		}
		if number == end.line {
			right = min(end.column, width)
		}
		if right <= left {
			continue
		}
		rows[index] = ansi.Cut(row, 0, left) + ansi.ResetStyle +
			styleSelection.Render(ansi.Strip(ansi.Cut(row, left, right))) +
			ansi.Cut(row, right, width)
	}
}

// paneArea is where a pane's selectable text is on screen: height rows from
// screen row top, width cells from column left, the first of them showing line
// offset of the pane's lineCount laid-out lines.
type paneArea struct {
	left, top, width, height int
	offset, lineCount        int
}

func (area paneArea) contains(x, y int) bool {
	return x >= area.left && x < area.left+area.width && y >= area.top && y < area.top+area.height
}

// pointAt is the point of the text at screen cell (x, y), held inside the
// area: past its sides a point is at the start or the end of the line, and
// below the last line it is on the last line.
func (area paneArea) pointAt(x, y int) selectionPoint {
	row := min(max(y-area.top, 0), area.height-1)
	return selectionPoint{
		line:   min(area.offset+row, max(area.lineCount-1, 0)),
		column: min(max(x-area.left, 0), area.width),
	}
}

// selectionPanes are the panes, in the order a press looks for the one it
// landed on.
var selectionPanes = []selectionPane{chatPane, approvalPane, inputPane}

// paneArea is where pane's selectable text is on screen, or false when the
// pane shows none.
func (m *Model) paneArea(pane selectionPane) (paneArea, bool) {
	switch pane {
	case chatPane:
		left, top := chatContentOrigin()
		area := paneArea{left: left, top: top, width: m.chat.Width, height: m.chat.Height,
			offset: m.chat.YOffset, lineCount: m.chat.TotalLineCount()}
		return area, area.lineCount > 0 && area.height > 0
	case approvalPane:
		return m.approvalBodyArea()
	case inputPane:
		return m.inputArea()
	}
	return paneArea{}, false
}

// paneLines returns lines [first, last) of pane's laid-out text, with what
// copying needs to know about each.
func (m *Model) paneLines(pane selectionPane, first, last int) ([]string, []lineInfo) {
	var lines []string
	var info []lineInfo
	switch pane {
	case chatPane:
		return m.chatLines(first, last)
	case approvalPane:
		lines, info = approvalBodyLines(m.approvalFields, m.approvalWidth())
	case inputPane:
		lines, info = m.input.rows()
	}
	last = min(last, len(lines))
	first = min(max(first, 0), last)
	return lines[first:last], info[first:last]
}

func (m *Model) scrollPane(pane selectionPane, delta int) {
	switch pane {
	case chatPane:
		if delta < 0 {
			m.chat.LineUp(-delta)
		} else {
			m.chat.LineDown(delta)
		}
	case approvalPane:
		m.scrollApproval(delta)
	}
}

// highlightPane highlights the selection on rows of pane as they are about to
// be drawn, when the selection is in that pane. rows start at line firstLine
// of the pane's text, with info alongside.
func (m *Model) highlightPane(pane selectionPane, rows []string, info []lineInfo, firstLine int) {
	if m.selection == nil || !m.selection.moved || m.selection.pane != pane {
		return
	}
	start, end := m.selection.span()
	highlightRows(rows, info, firstLine, start, end)
}

// copySelection copies the selected text as it reads on screen, without its
// colours.
func (m *Model) copySelection() tea.Cmd {
	start, end := m.selection.span()
	lines, info := m.paneLines(m.selection.pane, start.line, end.line+1)
	text := selectedText(lines, info, start.line, start, end)
	if strings.TrimSpace(text) == "" {
		return m.setToast("the selection is blank")
	}
	return m.copyToClipboard("selection", text)
}

// keysKeepingSelection are the keys that leave a selection in place: the one
// that copies it, and the ones that scroll the chat to see the rest of it.
// Any other key drops it before doing what it does.
var keysKeepingSelection = map[string]bool{
	"ctrl+y": true,
	"pgup":   true, "pgdown": true, "ctrl+home": true, "ctrl+end": true,
}

// startSelection begins a drag at screen cell (x, y) in the pane it is on,
// dropping any earlier selection either way.
func (m *Model) startSelection(x, y int) {
	m.selection = nil
	for _, pane := range selectionPanes {
		area, ok := m.paneArea(pane)
		if !ok || !area.contains(x, y) {
			continue
		}
		point := area.pointAt(x, y)
		m.selection = &textSelection{pane: pane, anchor: point, head: point}
		m.dragging = true
		if pane == chatPane {
			m.userScrolled = true
		}
		return
	}
}

// dragSelection carries a drag through to its release, reporting whether msg
// belonged to one.
func (m *Model) dragSelection(msg tea.MouseMsg) (bool, tea.Cmd) {
	if !m.dragging {
		return false, nil
	}
	switch msg.Action {
	case tea.MouseActionMotion:
		if m.selection != nil {
			m.extendSelection(msg.X, msg.Y)
		}
		return true, nil
	case tea.MouseActionRelease:
		return true, m.endSelection(msg.X, msg.Y)
	}
	return false, nil
}

// extendSelection moves the selection's head to screen cell (x, y), held
// inside its pane: past the top or bottom edge of a pane that scrolls, the
// pointer scrolls it a line.
func (m *Model) extendSelection(x, y int) {
	pane := m.selection.pane
	area, ok := m.paneArea(pane)
	if !ok {
		m.selection = nil
		return
	}
	switch {
	case y < area.top:
		m.scrollPane(pane, -1)
	case y >= area.top+area.height:
		m.scrollPane(pane, 1)
	}
	area, _ = m.paneArea(pane)
	if head := area.pointAt(x, y); head != m.selection.head {
		m.selection.head = head
		m.selection.moved = true
	}
}

// endSelection finishes a drag. One that never left the cell it started on was
// a click, and toggles the tool call under it.
func (m *Model) endSelection(x, y int) tea.Cmd {
	m.dragging = false
	selection := m.selection
	if selection == nil {
		return nil
	}
	if selection.pane == chatPane {
		m.userScrolled = !m.chat.AtBottom()
	}
	if selection.moved {
		return m.setToast("^y copies the selection")
	}
	m.selection = nil
	if entry := m.toolAt(x, y); selection.pane == chatPane && entry != nil {
		m.toggleTool(entry)
	}
	return nil
}

// chatView is the chat viewport as drawn, with the selection highlighted.
func (m *Model) chatView() string {
	view := m.chat.View()
	if m.selection == nil || m.selection.pane != chatPane {
		return view
	}
	rows := strings.Split(view, "\n")
	m.highlightPane(chatPane, rows, m.transcript.lineInfo(m.chat.YOffset, m.chat.YOffset+len(rows)), m.chat.YOffset)
	return strings.Join(rows, "\n")
}

// inputView is the input box as drawn, with the selection highlighted.
func (m *Model) inputView() string {
	view := m.input.View()
	if m.selection == nil || m.selection.pane != inputPane {
		return view
	}
	rows := strings.Split(view, "\n")
	_, info := m.input.rows()
	m.highlightPane(inputPane, rows, info, 0)
	return strings.Join(rows, "\n")
}

// inputArea is where the input box's text is, when there is text in it to
// select. Its rows are what it shows: once it scrolls, the rows scrolled away
// cannot be reached.
func (m *Model) inputArea() (paneArea, bool) {
	rows := m.input.Height()
	area := paneArea{
		left:      styleInputBorder.GetBorderLeftSize() + styleInputBorder.GetPaddingLeft(),
		top:       m.height - rows - styleInputBorder.GetBorderBottomSize(),
		width:     m.width - 2 - styleInputBorder.GetHorizontalPadding(),
		height:    rows,
		lineCount: rows,
	}
	return area, m.input.Value() != "" && m.width > 0
}

// chatLines returns lines [first, last) of the chat as laid out: the
// transcript's, then those of the reply still streaming in below it.
func (m *Model) chatLines(first, last int) ([]string, []lineInfo) {
	committed := m.transcript.lineCount
	lines, info := m.transcript.lines(first, min(last, committed))
	if last <= committed || m.pendingAs.Len() == 0 || m.chat.Width <= 0 {
		return lines, info
	}
	pendingText, pendingInfo := wrapText(m.pendingStyled(), m.chat.Width)
	pending := strings.Split(pendingText, "\n")
	for index := min(max(first-committed, 0), len(pending)); index < min(last-committed, len(pending)); index++ {
		lines = append(lines, pending[index])
		info = append(info, infoAt(pendingInfo, index))
	}
	return lines, info
}

// pendingLayout is the reply still streaming in, laid out at width as
// chatLines lays it out for copying. It is drawn again on every repaint while
// the reply streams, so it leaves out the line info only copying needs.
func (m *Model) pendingLayout(width int) string {
	return ansi.Wrap(m.pendingStyled(), width, "")
}

func (m *Model) pendingStyled() string {
	return styleAssist.Render(revealHiddenPlain(m.pendingAs.String()))
}
