package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/ruvolof/yaaih/internal/agent"
)

// approvalField is one top-level argument of a tool call, prepared for review.
// An empty name means the arguments were not a JSON object and value holds
// them whole.
type approvalField struct {
	name  string
	value string
}

// parseApprovalArgs splits a tool call's JSON arguments into fields in the
// order the model sent them. String values are decoded, so a command reads as
// the shell will run it — real line breaks, no \n or > escapes — rather
// than as one JSON line.
func parseApprovalArgs(args string) []approvalField {
	whole := []approvalField{{value: revealHidden(args)}}
	decoder := json.NewDecoder(strings.NewReader(args))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return whole
	}
	var fields []approvalField
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return whole
		}
		name, _ := token.(string)
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return whole
		}
		fields = append(fields, approvalField{name: revealHidden(name), value: revealHidden(argumentText(raw))})
	}
	return fields
}

func argumentText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		return string(raw)
	}
	return indented.String()
}

// approvalBodyLines lays the fields out at the given width. Every line of a
// value is numbered and long lines are wrapped exactly, keeping every space,
// onto unnumbered continuation rows: in a shell command a line break separates
// commands, so a wrap must never be mistaken for one. The numbers are a gutter
// in info, and the continuation rows join back onto their line when copied.
func approvalBodyLines(fields []approvalField, width int) (lines []string, info []lineInfo) {
	for _, field := range fields {
		if field.name != "" {
			lines = append(lines, styleTool.Render(field.name))
			info = append(info, lineInfo{})
		}
		valueLines := strings.Split(field.value, "\n")
		numberWidth := len(strconv.Itoa(len(valueLines)))
		textWidth := max(width-numberWidth-3, 10)
		for index, valueLine := range valueLines {
			segments := strings.Split(ansi.Hardwrap(valueLine, textWidth, true), "\n")
			segmentInfo := breaksIn(valueLine, segments)
			for row, segment := range segments {
				number := ""
				if row == 0 {
					number = strconv.Itoa(index + 1)
				}
				lines = append(lines, styleMuted.Render(fmt.Sprintf("%*s │ ", numberWidth, number))+segment)
				segmentInfo[row].gutter = numberWidth + 3
			}
			info = append(info, segmentInfo...)
		}
	}
	return lines, info
}

// approvalWidth is the panel's text width. The panel spans the screen like the
// input box under it and wraps at the same width.
func (m *Model) approvalWidth() int {
	return max(m.width-6, 20)
}

// approvalBodyHeight is how many argument lines the panel shows at once. The
// whole panel takes at most half the screen, so the conversation that led up
// to the call stays in view above it; what is left after the border, the
// title, the scroll hint and the key hints goes to the arguments.
func (m *Model) approvalBodyHeight() int {
	width := m.approvalWidth()
	chrome := styleApproval.GetVerticalFrameSize() + lipgloss.Height(m.approvalTitle(width)) + 1 +
		lipgloss.Height(m.approvalKeyHints(width))
	return max(m.height/2-chrome, 3)
}

func (m *Model) approvalMaxScroll() int {
	lines, _ := approvalBodyLines(m.approvalFields, m.approvalWidth())
	return max(len(lines)-m.approvalBodyHeight(), 0)
}

// approvalWindow is the argument lines [first, last) the panel shows, of
// count.
func (m *Model) approvalWindow(count int) (first, last int) {
	height := m.approvalBodyHeight()
	first = min(m.approvalScroll, max(count-height, 0))
	return first, min(first+height, count)
}

// approvalBodyArea is where the panel's arguments are on screen, when it is
// showing them.
func (m *Model) approvalBodyArea() (paneArea, bool) {
	if m.pendingApproval == nil || m.approvalCollapsed || m.width == 0 {
		return paneArea{}, false
	}
	width := m.approvalWidth()
	lines, _ := approvalBodyLines(m.approvalFields, width)
	first, last := m.approvalWindow(len(lines))
	area := paneArea{
		left:      styleApproval.GetBorderLeftSize() + styleApproval.GetPaddingLeft(),
		top:       m.approvalPanelTop() + styleApproval.GetBorderTopSize() + lipgloss.Height(m.approvalTitle(width)),
		width:     width,
		height:    last - first,
		offset:    first,
		lineCount: len(lines),
	}
	return area, area.height > 0
}

func (m *Model) scrollApproval(delta int) {
	m.approvalScroll = min(max(m.approvalScroll+delta, 0), m.approvalMaxScroll())
}

// openApproval docks the panel for request above the input. Every request
// starts with its arguments shown: hiding the previous call's script must not
// hide this one's.
func (m *Model) openApproval(request *agent.ApprovalRequest) {
	m.pendingApproval = request
	m.approvalFields = parseApprovalArgs(request.Args)
	m.approvalScroll = 0
	m.approvalCollapsed = false
	m.relayoutIfSized()
}

func (m *Model) closeApproval() {
	m.pendingApproval = nil
	m.approvalFields = nil
	m.relayoutIfSized()
}

func (m *Model) toggleApprovalArgs() {
	m.approvalCollapsed = !m.approvalCollapsed
	m.relayoutIfSized()
}

// handleApprovalKey answers or navigates the pending request, reporting
// whether key was one of the panel's.
func (m *Model) handleApprovalKey(key string) bool {
	request := m.pendingApproval
	switch key {
	case "y", "Y":
		request.Approve()
		m.closeApproval()
	case "a":
		request.ApproveTool()
		m.closeApproval()
	case "A":
		request.ApproveAll()
		m.closeApproval()
	case "n", "N":
		// Deny this call but let the agent keep iterating — the model may
		// try a different tool or approach.
		request.Deny()
		m.closeApproval()
	case "q", "Q", "esc":
		// Cancel the entire turn — break the model out of whatever loop it is
		// in and return control to the operator.
		request.Cancel()
		m.closeApproval()
	case "c", "C":
		m.toggleApprovalArgs()
	case "up", "k":
		m.scrollApproval(-1)
	case "down", "j":
		m.scrollApproval(1)
	case "shift+up":
		m.scrollApproval(-m.approvalBodyHeight())
	case "shift+down":
		m.scrollApproval(m.approvalBodyHeight())
	case "home", "g":
		m.approvalScroll = 0
	case "end", "G":
		m.approvalScroll = m.approvalMaxScroll()
	default:
		return false
	}
	return true
}

// keysBesideApproval are the keys that keep working while a request waits:
// the ones for reading the conversation that led to it, copying, quitting,
// looking around the sandbox before answering, and the shortcut list.
// Everything else is held back, so a keystroke meant for the panel never lands
// in the input box or sends a message.
var keysBesideApproval = map[string]bool{
	"ctrl+c": true, "ctrl+d": true, "ctrl+]": true, keysOverlayKey: true,
	"ctrl+y": true, "ctrl+t": true,
	"ctrl+o": true, "alt+up": true, "alt+down": true,
	"pgup": true, "pgdown": true, "ctrl+home": true, "ctrl+end": true,
}

// approvalPanelTop is the screen row of the panel's top border; the panel sits
// directly above the input box.
func (m *Model) approvalPanelTop() int {
	return m.height - (m.input.Height() + 2) - m.approvalHeight
}

// handleApprovalMouse scrolls the panel under the wheel, collapses or expands
// it on a click on its title, and starts selecting its arguments on a press
// on them, reporting whether the event was over the panel. Events anywhere
// else go on to the conversation.
func (m *Model) handleApprovalMouse(msg tea.MouseMsg) bool {
	top := m.approvalPanelTop()
	if msg.Y < top || msg.Y >= top+m.approvalHeight {
		return false
	}
	switch {
	case msg.Button == tea.MouseButtonWheelUp:
		m.scrollApproval(-3)
	case msg.Button == tea.MouseButtonWheelDown:
		m.scrollApproval(3)
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft &&
		msg.Y == top+styleApproval.GetBorderTopSize():
		m.toggleApprovalArgs()
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		m.startSelection(msg.X, msg.Y)
	}
	return true
}

func (m *Model) approvalTitle(width int) string {
	return ansi.Wrap(styleWarn.Render("⚠ Approval required")+styleMuted.Render(" · ")+
		styleAccent(revealHidden(m.pendingApproval.ToolName)), width, "")
}

func (m *Model) approvalKeyHints(width int) string {
	toggle := "[c] hide args"
	if m.approvalCollapsed {
		toggle = "[c] show args"
	}
	answers := styleMuted.Render("[y] approve   [n] deny   [q/esc] cancel turn   " + toggle)
	standing := styleMuted.Render(fmt.Sprintf("[a] always allow %s   [A] allow all tools   (this session)",
		revealHidden(m.pendingApproval.ToolName)))
	return ansi.Wrap(answers, width, "") + "\n" + ansi.Wrap(standing, width, "")
}

// renderApproval is the panel's content: the call, as much of its arguments as
// fits, and the keys that answer it.
func (m *Model) renderApproval() string {
	width := m.approvalWidth()
	lines, info := approvalBodyLines(m.approvalFields, width)
	sections := []string{m.approvalTitle(width)}
	if m.approvalCollapsed {
		sections = append(sections, styleMuted.Render(ansi.Truncate(
			fmt.Sprintf("arguments hidden (%d lines)", len(lines)), width, "…")))
	} else {
		first, last := m.approvalWindow(len(lines))
		visible := slices.Clone(lines[first:last])
		m.highlightPane(approvalPane, visible, info[first:last], first)
		sections = append(sections, visible...)
		if len(lines) > m.approvalBodyHeight() {
			sections = append(sections, styleWarn.Render(ansi.Truncate(fmt.Sprintf(
				"lines %d–%d of %d · ↑/↓ shift+↑/↓ home/end or wheel to scroll", first+1, last, len(lines)), width, "…")))
		}
	}
	sections = append(sections, m.approvalKeyHints(width))
	return strings.Join(sections, "\n")
}

func (m *Model) renderApprovalPanel() string {
	return styleApproval.Width(m.width - 2).Render(m.renderApproval())
}
