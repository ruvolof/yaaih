package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// unwrappedWidth lays an entry out without wrapping, for copying it out of the
// terminal rather than drawing it.
const unwrappedWidth = 1 << 20

// transcript is the chat history as a list of entries, each laid out at the
// viewport width and cached, so that one entry can change — a tool call
// expanded, collapsed, selected, or answered — without re-wrapping the rest.
//
// Appending an entry is the streaming hot path and only lays out that entry;
// changing one re-lays it out and re-joins the cached text of all of them.
//
// Replies are the exception to laying everything out on a width change:
// rendering Markdown costs far more than wrapping text, so a reply keeps its
// last rendering re-wrapped as a stand-in until renderReplies or
// renderStaleNear draws it at the new width.
type transcript struct {
	entries []*chatEntry

	// width is what every entry's layout was computed at; 0 until the
	// viewport has a size, and nothing is laid out until then.
	width int
	// body is every entry's layout joined by newlines: what the viewport shows
	// above any reply still streaming in.
	body      strings.Builder
	lineCount int

	// selected is the tool call alt+↑/↓ has moved to, or nil.
	selected *chatEntry

	// changed and changedFrom say which lines may no longer hold the text they
	// held when takeChange was last called: everything from changedFrom on.
	changed     bool
	changedFrom int

	markdown markdownRenderer
}

// chatEntry is one block of the transcript: fixed styled text, a completed
// assistant reply kept as the Markdown it arrived in, or a tool call, which the
// operator can expand to see in full.
type chatEntry struct {
	text     string
	reply    string
	tool     *toolCall
	expanded bool

	// rendered is the reply as glamour drew it at renderedWidth, which is 0
	// until it has been drawn at all.
	rendered      string
	renderedInfo  []lineInfo
	renderedWidth int

	index  int
	layout string
	// layoutInfo describes layout's lines for copying; nil when none of them
	// was broken to fit or has a gutter.
	layoutInfo []lineInfo
	firstLine  int
	lineCount  int
}

// toolCall is a call and, once it has one, its reply. result is the reply
// exactly as the model received it.
type toolCall struct {
	id       string
	name     string
	args     string
	result   string
	answered bool
	failed   bool
}

func (t *transcript) appendText(text string) *chatEntry {
	return t.appendEntry(&chatEntry{text: text})
}

func (t *transcript) appendReply(markdown string) *chatEntry {
	return t.appendEntry(&chatEntry{reply: markdown})
}

func (t *transcript) appendTool(call *toolCall) *chatEntry {
	return t.appendEntry(&chatEntry{tool: call})
}

func (t *transcript) appendEntry(entry *chatEntry) *chatEntry {
	entry.index = len(t.entries)
	t.entries = append(t.entries, entry)
	if t.width > 0 {
		if entry.reply != "" {
			t.renderReply(entry)
		}
		t.layOut(entry, t.width)
		entry.firstLine = t.lineCount
		entry.lineCount = strings.Count(entry.layout, "\n") + 1
		// The new entry takes the lines a reply still streaming in was drawn on.
		t.noteChange(entry.firstLine)
		if entry.index > 0 {
			t.body.WriteString("\n")
		}
		t.body.WriteString(entry.layout)
		t.lineCount += entry.lineCount
	}
	return entry
}

// setWidth lays every entry out again at width. Does nothing when the width is
// unchanged, so it is safe to call on every render.
func (t *transcript) setWidth(width int) {
	if width == t.width {
		return
	}
	t.width = width
	t.noteChange(0)
	for _, entry := range t.entries {
		t.layOut(entry, width)
	}
	t.rejoin()
}

// refresh lays out the given entries again after their state changed.
func (t *transcript) refresh(changed ...*chatEntry) {
	if t.width <= 0 {
		return
	}
	for _, entry := range changed {
		if entry != nil {
			t.layOut(entry, t.width)
			t.noteChange(entry.firstLine)
		}
	}
	t.rejoin()
}

func (t *transcript) rejoin() {
	t.body.Reset()
	t.lineCount = 0
	for index, entry := range t.entries {
		if index > 0 {
			t.body.WriteString("\n")
		}
		t.body.WriteString(entry.layout)
		entry.firstLine = t.lineCount
		entry.lineCount = strings.Count(entry.layout, "\n") + 1
		t.lineCount += entry.lineCount
	}
}

func (t *transcript) reset() {
	t.entries = nil
	t.body.Reset()
	t.lineCount = 0
	t.selected = nil
	t.noteChange(0)
}

func (t *transcript) noteChange(line int) {
	if !t.changed || line < t.changedFrom {
		t.changedFrom = line
	}
	t.changed = true
}

// takeChange reports the first line that may have changed since the last
// call, if any did.
func (t *transcript) takeChange() (line int, changed bool) {
	line, changed = t.changedFrom, t.changed
	t.changed = false
	return line, changed
}

// lines returns the laid-out lines [first, last), read from the entries they
// belong to, with what copying needs to know about each.
func (t *transcript) lines(first, last int) ([]string, []lineInfo) {
	var lines []string
	var info []lineInfo
	for line := max(first, 0); line < last; {
		entry := t.entryAtLine(line)
		if entry == nil {
			break
		}
		entryLines := strings.Split(entry.layout, "\n")
		from, to := line-entry.firstLine, min(last-entry.firstLine, len(entryLines))
		lines = append(lines, entryLines[from:to]...)
		for index := from; index < to; index++ {
			info = append(info, infoAt(entry.layoutInfo, index))
		}
		line = entry.firstLine + to
	}
	return lines, info
}

// lineInfo returns what copying needs to know about lines [first, last), as
// lines does, without reading the lines themselves.
func (t *transcript) lineInfo(first, last int) []lineInfo {
	var info []lineInfo
	for line := max(first, 0); line < min(last, t.lineCount); line++ {
		entry := t.entryAtLine(line)
		info = append(info, infoAt(entry.layoutInfo, line-entry.firstLine))
	}
	return info
}

func (t *transcript) layOut(entry *chatEntry, width int) {
	switch {
	case entry.tool != nil:
		entry.layout, entry.layoutInfo = entry.tool.layOut(width, entry.expanded, entry == t.selected)
	case entry.reply != "":
		entry.layout, entry.layoutInfo = entry.replyLayout(width)
	default:
		entry.layout, entry.layoutInfo = wrapText(entry.text, width)
	}
}

// replyLayout is the reply as glamour drew it at width or, until it has been,
// a stand-in: its last rendering re-wrapped to fit, which is close enough in
// shape that little moves when the real one replaces it.
func (entry *chatEntry) replyLayout(width int) (string, []lineInfo) {
	switch entry.renderedWidth {
	case width:
		return entry.rendered, entry.renderedInfo
	case 0:
		return plainReply(entry.reply, width)
	default:
		return wrapText(entry.rendered, width)
	}
}

func (t *transcript) renderReply(entry *chatEntry) {
	entry.rendered, entry.renderedInfo = t.markdown.render(entry.reply, t.width)
	entry.renderedWidth = t.width
	entry.layout, entry.layoutInfo = entry.rendered, entry.renderedInfo
}

func (t *transcript) showsStandIn(entry *chatEntry) bool {
	return entry.reply != "" && entry.renderedWidth != t.width
}

func (t *transcript) hasStandIns() bool {
	for _, entry := range t.entries {
		if t.showsStandIn(entry) {
			return true
		}
	}
	return false
}

// renderReplies draws every stand-in on lines [first, last) at the current
// width and reports whether there was one.
func (t *transcript) renderReplies(first, last int) bool {
	if t.width <= 0 {
		return false
	}
	rendered := false
	for _, entry := range t.entries {
		onScreen := entry.firstLine < last && entry.firstLine+entry.lineCount > first
		if onScreen && t.showsStandIn(entry) {
			t.renderReply(entry)
			t.noteChange(entry.firstLine)
			rendered = true
		}
	}
	if rendered {
		t.rejoin()
	}
	return rendered
}

// renderStaleNear draws stand-ins at the current width, nearest to line
// first, until budget is spent. It always draws at least one if any are left.
func (t *transcript) renderStaleNear(line int, budget time.Duration) {
	if t.width <= 0 || len(t.entries) == 0 {
		return
	}
	center := len(t.entries) - 1
	if entry := t.entryAtLine(line); entry != nil {
		center = entry.index
	}
	deadline := time.Now().Add(budget)
	rendered := false
	for distance := 0; distance < len(t.entries); distance++ {
		for _, index := range []int{center + distance, center - distance} {
			if index >= 0 && index < len(t.entries) && t.showsStandIn(t.entries[index]) {
				t.renderReply(t.entries[index])
				t.noteChange(t.entries[index].firstLine)
				rendered = true
			}
		}
		if rendered && time.Now().After(deadline) {
			break
		}
	}
	if rendered {
		t.rejoin()
	}
}

// plainText is the whole transcript without styling or wrapping, replies as
// their Markdown source and tool calls in full.
func (t *transcript) plainText() string {
	var plain strings.Builder
	for index, entry := range t.entries {
		if index > 0 {
			plain.WriteString("\n")
		}
		text := entry.text
		if entry.reply != "" {
			text = entry.reply
		}
		if entry.tool != nil {
			text, _ = entry.tool.layOut(unwrappedWidth, true, false)
		}
		plain.WriteString(stripANSI(text))
	}
	return plain.String()
}

// entryAtLine returns the entry drawn on the given line of the laid-out body,
// or nil past its end.
func (t *transcript) entryAtLine(line int) *chatEntry {
	if line < 0 || line >= t.lineCount {
		return nil
	}
	index := sort.Search(len(t.entries), func(i int) bool {
		return t.entries[i].firstLine > line
	}) - 1
	if index < 0 {
		return nil
	}
	return t.entries[index]
}

// unansweredTool finds the call a reply belongs to: the most recent one with
// that id still waiting for its reply.
func (t *transcript) unansweredTool(id string) *chatEntry {
	for index := len(t.entries) - 1; index >= 0; index-- {
		entry := t.entries[index]
		if entry.tool != nil && !entry.tool.answered && entry.tool.id == id {
			return entry
		}
	}
	return nil
}

// toolBefore returns the closest tool call above index, or nil.
func (t *transcript) toolBefore(index int) *chatEntry {
	for index--; index >= 0; index-- {
		if t.entries[index].tool != nil {
			return t.entries[index]
		}
	}
	return nil
}

// toolAfter returns the closest tool call below index, or nil.
func (t *transcript) toolAfter(index int) *chatEntry {
	for index++; index < len(t.entries); index++ {
		if t.entries[index].tool != nil {
			return t.entries[index]
		}
	}
	return nil
}

// toolResultIndent sets the expanded arguments and reply off from the call
// line above them.
const toolResultIndent = "    "

// layOut draws a call collapsed — one line for the call, one for a preview
// of the reply — or expanded, with every argument and the whole reply. The
// indent and the line numbers in front of the arguments and the reply are
// gutters, left out when the text is copied.
func (call *toolCall) layOut(width int, expanded, selected bool) (string, []lineInfo) {
	marker := "▸"
	if expanded {
		marker = "▾"
	}
	heading := styleTool.Render(marker + " " + call.name)
	if selected {
		heading = styleSelected.Render(marker + " " + call.name)
	}

	if !expanded {
		lines := []string{heading + styleTool.Render(" "+truncate(call.args, 200))}
		if call.answered {
			lines = append(lines, call.statusLine(" "+truncate(call.replyWithoutErrorPrefix(), 600)))
		}
		return wrapText(strings.Join(lines, "\n"), width)
	}

	innerWidth := max(width-len(toolResultIndent), 10)
	lines, info := wrapEach(heading, func(line string) string { return ansi.Wrap(line, width, "") })
	info = growInfo(info, len(lines))
	argumentLines, argumentInfo := approvalBodyLines(parseApprovalArgs(call.args), innerWidth)
	for index, argumentLine := range argumentLines {
		lines = append(lines, toolResultIndent+argumentLine)
		argumentInfo[index].gutter += len(toolResultIndent)
	}
	info = append(info, argumentInfo...)
	if !call.answered {
		return strings.Join(lines, "\n"), info
	}

	reply := strings.TrimSuffix(revealHidden(call.result), "\n")
	if call.result == "" {
		lines = append(lines, call.statusLine(" (no output)"))
		return strings.Join(lines, "\n"), append(info, lineInfo{})
	}
	lines = append(lines, call.statusLine(fmt.Sprintf(" %d lines", strings.Count(reply, "\n")+1)))
	info = append(info, lineInfo{})
	segments, segmentInfo := wrapEach(reply, func(line string) string { return ansi.Hardwrap(line, innerWidth, true) })
	segmentInfo = growInfo(segmentInfo, len(segments))
	for index, segment := range segments {
		lines = append(lines, toolResultIndent+styleToolRes.Render(segment))
		segmentInfo[index].gutter = len(toolResultIndent)
	}
	return strings.Join(lines, "\n"), append(info, segmentInfo...)
}

func (call *toolCall) statusLine(detail string) string {
	if call.failed {
		return styleErr.Render("  ✗" + detail)
	}
	return styleToolRes.Render("  ✓" + detail)
}

// replyWithoutErrorPrefix is the reply for the one-line preview, where the ✗
// already says the call failed.
func (call *toolCall) replyWithoutErrorPrefix() string {
	if !call.failed {
		return call.result
	}
	rest, _ := strings.CutPrefix(call.result, "error:")
	return strings.TrimSpace(rest)
}
