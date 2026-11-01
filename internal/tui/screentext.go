package tui

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// The renderer places every row by its own count of the cells each grapheme
// cluster takes, and assumes the terminal agrees. Where the terminal draws a
// row wider than that count, the row wraps onto the next, the screen scrolls,
// and the renderer stays one row out until a full repaint: a doubled status
// line, the input box pushed off the bottom. Where it draws a row narrower,
// only the borders to its right shift. Text from outside — the model, a tool,
// the operator — reaches the screen through fitForScreen, which leaves nothing
// a terminal draws wider than the renderer counts it.
//
// Terminals do not agree on clusters. Most draw code point by code point, so
// a family emoji joined with ZWJs is three emoji wide, a skin-toned thumb is
// a thumb and a swatch, and a Hindi vowel sign takes a cell of its own; the
// renderer counts each of those as one cluster. Splitting such a cluster with
// clusterBreak makes the renderer count it the way such a terminal draws it,
// without moving any code point to another cell.
//
// A terminal set to draw East Asian ambiguous-width characters wide is out of
// reach: the box-drawing borders are such characters themselves.

// clusterBreak separates the code points of a grapheme cluster whose width
// terminals disagree on. A zero-width space in the source is revealed, so one
// on screen is always a clusterBreak, and copying takes them back out.
const clusterBreak = "\u200b"

const (
	zeroWidthJoiner = '\u200d'
	tabStop         = 8
)

// sgrSequence is a Select Graphic Rendition sequence: colours and text
// attributes, never a cursor movement.
var sgrSequence = regexp.MustCompile(`^\x1b\[[0-9;:]*m`)

// revealHidden makes characters that would act on the terminal, or render as
// nothing, show up as escapes, and fits the rest for the screen. Decoded
// arguments are model output: a carriage return, an ANSI sequence or a bidi
// override in a command could otherwise redraw the panel and make the operator
// approve something other than what they read.
func revealHidden(text string) string {
	return fitForScreen(text, warnMarker, false)
}

// revealHiddenPlain is revealHidden for text styled as a whole once it is
// fitted, or rendered as Markdown: the styling of an escape would end the
// style around it.
func revealHiddenPlain(text string) string {
	return fitForScreen(text, plainMarker, false)
}

// revealHiddenStyled is revealHiddenPlain for text that already carries its
// own colours: SGR sequences are kept, and any other escape is revealed.
func revealHiddenStyled(text string) string {
	return fitForScreen(text, plainMarker, true)
}

func warnMarker(escaped string) string {
	return styleWarn.Render(escaped)
}

func plainMarker(escaped string) string {
	return escaped
}

// withoutClusterBreaks is text as copied: without the breaks fitForScreen put
// into it.
func withoutClusterBreaks(text string) string {
	return strings.ReplaceAll(text, clusterBreak, "")
}

// fitForScreen reveals what is hidden or acts on the terminal — control and
// format characters, invalid bytes, variation selectors that select nothing —
// as escapes drawn through mark, expands tabs to the terminal's tab stops,
// drops the presentation selectors and emoji joiners that only some terminals
// honour, and splits the clusters terminals disagree on the width of.
func fitForScreen(text string, mark func(string) string, keepStyling bool) string {
	if isPrintableASCII(text) {
		return text
	}
	var screen strings.Builder
	screen.Grow(len(text))
	column := 0
	reveal := func(escaped string) {
		screen.WriteString(mark(escaped))
		column += len(escaped)
	}
	for rest := text; rest != ""; {
		if keepStyling {
			if sequence := sgrSequence.FindString(rest); sequence != "" {
				screen.WriteString(sequence)
				rest = rest[len(sequence):]
				continue
			}
		}
		first := rest[0]
		if first >= ' ' && first < 0x7f && (len(rest) == 1 || rest[1] < 0x80) {
			screen.WriteByte(first)
			column++
			rest = rest[1:]
			continue
		}
		character, size := utf8.DecodeRuneInString(rest)
		switch {
		case character == '\n':
			screen.WriteByte('\n')
			column = 0
		case character == '\t':
			spaces := tabStop - column%tabStop
			screen.WriteString(strings.Repeat(" ", spaces))
			column += spaces
		case character == utf8.RuneError && size == 1:
			reveal(fmt.Sprintf(`\x%02x`, first))
		case isHidden(character), isVariationSelector(character):
			reveal(escapeRune(character))
		default:
			cluster, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
			size = len(cluster)
			column += writeCluster(&screen, cluster, reveal)
		}
		rest = rest[size:]
	}
	return screen.String()
}

// writeCluster writes one grapheme cluster, revealing the hidden characters
// it carries and dropping the selectors and joiners only some terminals
// honour, and returns the cells the rest takes.
func writeCluster(screen *strings.Builder, cluster string, reveal func(string)) int {
	characters := []rune(cluster)
	var kept []rune
	cells := 0
	flush := func() {
		cells += writeAgreed(screen, kept)
		kept = kept[:0]
	}
	for index, character := range characters {
		switch {
		case character == zeroWidthJoiner && joinsPictographs(characters, index):
		case isPresentationSelector(character) && index > 0 && !isVariationSelector(characters[index-1]):
		case isHidden(character), isVariationSelector(character):
			flush()
			reveal(escapeRune(character))
		default:
			kept = append(kept, character)
		}
	}
	flush()
	return cells
}

// writeAgreed writes characters as they are when terminals agree on their
// width, and split by clusterBreak when they do not. It returns the cells they
// take. Characters that lost what they were joined to — to an escape, a tab or
// a dropped joiner — are kept from joining whatever was written before them.
func writeAgreed(screen *strings.Builder, characters []rune) int {
	if len(characters) > 0 && joinsWhatPrecedes(characters[0]) {
		screen.WriteString(clusterBreak)
	}
	if !needsSplit(characters) {
		text := string(characters)
		screen.WriteString(text)
		return ansi.StringWidth(text)
	}
	var split strings.Builder
	for index, character := range characters {
		if index > 0 {
			split.WriteString(clusterBreak)
		}
		split.WriteRune(character)
	}
	screen.WriteString(split.String())
	return ansi.StringWidth(split.String())
}

// needsSplit reports whether the renderer's count of the cells a cluster takes
// differs from a terminal's that draws it code point by code point. A spacing
// mark always takes a cell of its own there, even where the width tables give
// it none. Hangul syllables spelled out in jamo are the exception: terminals
// join those into one syllable as the renderer counts it.
func needsSplit(characters []rune) bool {
	if len(characters) < 2 || allHangul(characters) {
		return false
	}
	for _, character := range characters[1:] {
		if unicode.Is(unicode.Mc, character) {
			return true
		}
	}
	text := string(characters)
	return ansi.StringWidth(text) != ansi.StringWidthWc(text)
}

// joinsWhatPrecedes reports whether character joins the one before it into a
// cluster: a combining mark, a vowel sign, a skin tone.
func joinsWhatPrecedes(character rune) bool {
	cluster, _ := ansi.FirstGraphemeCluster("x"+string(character), ansi.GraphemeWidth)
	return len(cluster) > 1
}

func allHangul(characters []rune) bool {
	for _, character := range characters {
		if !unicode.Is(unicode.Hangul, character) {
			return false
		}
	}
	return true
}

// isHidden reports whether character must be shown as an escape: it acts on
// the terminal, draws as nothing, or is newer than Go's Unicode tables. Those
// are mostly marks the width tables count as no cells, where a terminal that
// knows them gives them one; newer emoji, which both count as two, are kept.
func isHidden(character rune) bool {
	if unicode.IsControl(character) || unicode.In(character, unicode.Cf, unicode.Zl, unicode.Zp) {
		return true
	}
	unassigned := !unicode.IsGraphic(character) && !unicode.Is(unicode.Co, character)
	return unassigned && ansi.StringWidth(string(character)) < 2
}

func isVariationSelector(character rune) bool {
	return (character >= '\ufe00' && character <= '\ufe0f') || (character >= '\U000e0100' && character <= '\U000e01ef')
}

// isPresentationSelector reports whether character asks for the text or the
// emoji form of the one before it: something a terminal honours or ignores
// at will, and the renderer always counts.
func isPresentationSelector(character rune) bool {
	return character == '\ufe0e' || character == '\ufe0f'
}

// joinsPictographs reports whether the joiner at index sits between two
// pictographs, as in an emoji sequence, rather than smuggled into text.
func joinsPictographs(characters []rune, index int) bool {
	before := index - 1
	for before >= 0 && (isVariationSelector(characters[before]) || unicode.Is(unicode.Sk, characters[before])) {
		before--
	}
	after := index + 1
	return before >= 0 && after < len(characters) &&
		unicode.Is(unicode.So, characters[before]) && unicode.Is(unicode.So, characters[after])
}

func isPrintableASCII(text string) bool {
	for index := 0; index < len(text); index++ {
		if character := text[index]; (character < ' ' && character != '\n') || character >= 0x7f {
			return false
		}
	}
	return true
}

func escapeRune(character rune) string {
	switch {
	case character < 0x80:
		return fmt.Sprintf(`\x%02x`, character)
	case character <= 0xffff:
		return fmt.Sprintf(`\u%04x`, character)
	default:
		return fmt.Sprintf(`\U%08x`, character)
	}
}
