package tui

import (
	"fmt"
	"sort"
	"strings"

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
}

// chatEntry is one block of the transcript: either fixed styled text or a tool
// call, which the operator can expand to see in full.
type chatEntry struct {
	text     string
	tool     *toolCall
	expanded bool

	index     int
	layout    string
	firstLine int
	lineCount int
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

func (t *transcript) appendTool(call *toolCall) *chatEntry {
	return t.appendEntry(&chatEntry{tool: call})
}

func (t *transcript) appendEntry(entry *chatEntry) *chatEntry {
	entry.index = len(t.entries)
	t.entries = append(t.entries, entry)
	if t.width > 0 {
		entry.layout = t.layOut(entry, t.width)
		entry.firstLine = t.lineCount
		entry.lineCount = strings.Count(entry.layout, "\n") + 1
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
	for _, entry := range t.entries {
		entry.layout = t.layOut(entry, width)
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
			entry.layout = t.layOut(entry, t.width)
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
}

func (t *transcript) layOut(entry *chatEntry, width int) string {
	if entry.tool != nil {
		return entry.tool.layOut(width, entry.expanded, entry == t.selected)
	}
	return ansi.Wrap(entry.text, width, "")
}

// plainText is the whole transcript without styling or wrapping, tool calls
// in full.
func (t *transcript) plainText() string {
	var plain strings.Builder
	for index, entry := range t.entries {
		if index > 0 {
			plain.WriteString("\n")
		}
		text := entry.text
		if entry.tool != nil {
			text = entry.tool.layOut(unwrappedWidth, true, false)
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
// of the reply — or expanded, with every argument and the whole reply.
func (call *toolCall) layOut(width int, expanded, selected bool) string {
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
		return ansi.Wrap(strings.Join(lines, "\n"), width, "")
	}

	innerWidth := max(width-len(toolResultIndent), 10)
	lines := []string{ansi.Wrap(heading, width, "")}
	for _, argumentLine := range approvalBodyLines(parseApprovalArgs(call.args), innerWidth) {
		lines = append(lines, toolResultIndent+argumentLine)
	}
	if !call.answered {
		return strings.Join(lines, "\n")
	}

	reply := revealHidden(call.result)
	replyLines := strings.Split(strings.TrimSuffix(reply, "\n"), "\n")
	if reply == "" {
		lines = append(lines, call.statusLine(" (no output)"))
		return strings.Join(lines, "\n")
	}
	lines = append(lines, call.statusLine(fmt.Sprintf(" %d lines", len(replyLines))))
	for _, replyLine := range replyLines {
		for _, segment := range strings.Split(ansi.Hardwrap(replyLine, innerWidth, true), "\n") {
			lines = append(lines, toolResultIndent+styleToolRes.Render(segment))
		}
	}
	return strings.Join(lines, "\n")
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
