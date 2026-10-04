package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	glamourstyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// markdownRenderer draws completed assistant replies as Markdown. It keeps one
// glamour renderer for the width the transcript is laid out at, since every
// reply is rendered at that width and building a renderer parses its style.
type markdownRenderer struct {
	width    int
	renderer *glamour.TermRenderer
}

// render draws source at width. A reply glamour cannot render is shown as the
// plain text it was streamed as.
func (md *markdownRenderer) render(source string, width int) string {
	if md.renderer == nil || md.width != width {
		renderer, err := glamour.NewTermRenderer(
			glamour.WithStyles(replyStyle()),
			glamour.WithWordWrap(width),
			glamour.WithColorProfile(lipgloss.ColorProfile()),
		)
		if err != nil {
			log.Warn("markdown renderer unavailable", "err", err)
			return plainReply(source, width)
		}
		md.renderer = renderer
		md.width = width
	}
	rendered, err := md.renderer.Render(source)
	if err != nil {
		log.Warn("markdown render failed", "err", err)
		return plainReply(source, width)
	}
	// Glamour wraps only between words, so a token longer than the width — a
	// path, a URL, a line of code — would overrun the viewport.
	return ansi.Hardwrap(trimRendered(rendered), width, true)
}

func plainReply(source string, width int) string {
	return ansi.Wrap(styleAssist.Render(source), width, "")
}

// replyStyle is glamour's Tokyo Night style, which shares the app's palette,
// with the document's margins and blank lines taken off so a reply lines up
// with the rest of the transcript. Body text takes the colour the reply
// streamed in, so it does not shift when the reply is committed.
func replyStyle() glamouransi.StyleConfig {
	style := glamourstyles.TokyoNightStyleConfig
	noMargin := uint(0)
	bodyColor := string(colorFG)
	style.Document.Margin = &noMargin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.Document.Color = &bodyColor
	style.List.Color = &bodyColor
	return style
}

// trimRendered drops the spaces glamour pads every line out to the full width
// with, and the blank line it ends on.
func trimRendered(rendered string) string {
	lines := strings.Split(rendered, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight(line, " ")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}
