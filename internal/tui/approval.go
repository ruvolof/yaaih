package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
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

// revealHidden makes characters that would act on the terminal, or render as
// nothing, show up as escapes. Decoded arguments are model output: a carriage
// return, an ANSI sequence or a bidi override in a command could otherwise
// redraw the modal and make the operator approve something other than what
// they read.
func revealHidden(text string) string {
	var revealed strings.Builder
	for _, character := range text {
		switch {
		case character == '\n':
			revealed.WriteRune(character)
		case character == '\t':
			revealed.WriteString("    ")
		case unicode.IsControl(character), unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp):
			revealed.WriteString(styleWarn.Render(escapeRune(character)))
		default:
			revealed.WriteRune(character)
		}
	}
	return revealed.String()
}

func escapeRune(character rune) string {
	if character < 0x80 {
		return fmt.Sprintf(`\x%02x`, character)
	}
	return fmt.Sprintf(`\u%04x`, character)
}

// approvalBodyLines lays the fields out at the given width. Every line of a
// value is numbered and long lines are wrapped exactly, keeping every space,
// onto unnumbered continuation rows: in a shell command a line break separates
// commands, so a wrap must never be mistaken for one.
func approvalBodyLines(fields []approvalField, width int) []string {
	var lines []string
	for _, field := range fields {
		if field.name != "" {
			lines = append(lines, styleTool.Render(field.name))
		}
		valueLines := strings.Split(field.value, "\n")
		numberWidth := len(strconv.Itoa(len(valueLines)))
		textWidth := max(width-numberWidth-3, 10)
		for index, valueLine := range valueLines {
			for row, segment := range strings.Split(ansi.Hardwrap(valueLine, textWidth, true), "\n") {
				number := ""
				if row == 0 {
					number = strconv.Itoa(index + 1)
				}
				lines = append(lines, styleMuted.Render(fmt.Sprintf("%*s │ ", numberWidth, number))+segment)
			}
		}
	}
	return lines
}

// approvalWidth is the modal's text width: the screen less a two-column margin
// each side, the double border and the horizontal padding.
func (m *Model) approvalWidth() int {
	return max(m.width-10, 20)
}

// approvalBodyHeight is how many argument lines fit: the screen less a one-row
// margin, the border, the padding and the eight rows of header and key hints.
func (m *Model) approvalBodyHeight() int {
	return max(m.height-14, 3)
}

func (m *Model) approvalMaxScroll() int {
	lines := approvalBodyLines(m.approvalFields, m.approvalWidth())
	return max(len(lines)-m.approvalBodyHeight(), 0)
}

func (m *Model) scrollApproval(delta int) {
	m.approvalScroll = min(max(m.approvalScroll+delta, 0), m.approvalMaxScroll())
}

func (m *Model) renderApproval() string {
	request := m.pendingApproval
	width := m.approvalWidth()
	lines := approvalBodyLines(m.approvalFields, width)
	height := m.approvalBodyHeight()
	first := min(m.approvalScroll, max(len(lines)-height, 0))
	last := min(first+height, len(lines))

	argsHeader := "Args:"
	if len(lines) > height {
		argsHeader += " " + styleWarn.Render(fmt.Sprintf(
			"lines %d–%d of %d · ↑/↓ pgup/pgdn home/end to scroll", first+1, last, len(lines)))
	}
	return fmt.Sprintf(
		"%s\n\nTool: %s\n\n%s\n%s\n\n%s\n%s",
		styleHeader.Render("Approval required"),
		styleAccent(revealHidden(request.ToolName)),
		ansi.Wrap(argsHeader, width, ""),
		strings.Join(lines[first:last], "\n"),
		ansi.Wrap(styleMuted.Render("[y] approve   [n] deny   [q/esc] cancel turn"), width, ""),
		ansi.Wrap(styleMuted.Render(fmt.Sprintf("[a] always allow %s   [A] allow all tools   (this session)", revealHidden(request.ToolName))), width, ""),
	)
}
