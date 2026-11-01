package tui

import (
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// binaryToolOutput is what a tool returned when the agent read an encrypted
// file back as text: lossy decoding left replacement characters, and raw
// control bytes came through with them. Vertical tab and form feed move the
// cursor down a row, and shift-in switches the character set.
const binaryToolOutput = "NODE_ENV=production\nM��S�\x11\x0bQ\x17�T\x0c\x12W\x0f�\x02\x10\u0091\x1e\x1b[2Jdone"

// mixedScriptReply is text whose width terminals disagree on: emoji joined
// into one glyph, presentation selectors, skin tones, keycaps, the vowel
// signs and conjuncts of Indic scripts, halfwidth voiced marks, and a tab.
const mixedScriptReply = "Family 👨\u200d👩\u200d👧\u200d👦 coder 🧑🏽\u200d💻 thumbs 👍🏿 warning ⚠\ufe0f ok ✔\ufe0f keycap 2\ufe0f\u20e3\n" +
	"Hindi नमस्ते दुनिया, हिन्दी भाषा बहुत सुन्दर है। Bengali বাংলা ভাষা Tamil தமிழ் மொழி\n" +
	"Kannada ಕನ್ನಡ ಭಾಷೆ kana ｶﾞｷﾞｸﾞ Thai กำ\tcolumn"

func TestControlBytesAreRevealed(t *testing.T) {
	shown := revealHiddenPlain(binaryToolOutput)
	for _, character := range shown {
		if unicode.IsControl(character) && character != '\n' {
			t.Fatalf("%q still carries control character %U", shown, character)
		}
	}
	for _, want := range []string{`\x0b`, `\x0c`, `\x0f`, `\u0091`, `\x1b[2J`, "M��S"} {
		if !strings.Contains(shown, want) {
			t.Errorf("%q does not show %q", shown, want)
		}
	}
}

func TestInvalidBytesAreRevealed(t *testing.T) {
	if shown := revealHiddenPlain("ok \xff\xfe end"); shown != `ok \xff\xfe end` {
		t.Errorf("shown as %q", shown)
	}
}

// A terminal moves a tab to the next stop, and the renderer counts it as no
// cells at all.
func TestTabsExpandToTabStops(t *testing.T) {
	shown := revealHiddenPlain("a\tbb\tccc\tdddddddd\te\nf\tg")
	want := "a       bb      ccc     dddddddd        e\nf       g"
	if shown != want {
		t.Errorf("shown as %q, want %q", shown, want)
	}
}

func TestTabStopsCountCellsNotBytes(t *testing.T) {
	if shown := revealHiddenPlain("中文\tx"); shown != "中文    x" {
		t.Errorf("shown as %q", shown)
	}
}

// Emoji selectors and joiners only change the glyph on terminals that honour
// them. Dropped, the emoji draw as the separate pictures most terminals show.
func TestEmojiSelectorsAndJoinersAreDropped(t *testing.T) {
	for source, want := range map[string]string{
		"⚠\ufe0f ✔\ufe0f":       "⚠ ✔",
		"👨\u200d👩\u200d👧":       "👨👩👧",
		"🏳\ufe0f\u200d🌈":        "🏳🌈",
		"2\ufe0f\u20e3":         "2\u20e3",
		"👩\u200d❤\ufe0f\u200d👨": "👩❤👨",
	} {
		if shown := revealHiddenPlain(source); shown != want {
			t.Errorf("%q shown as %q, want %q", source, shown, want)
		}
	}
}

// Zero-width characters outside an emoji, and variation selectors that select
// nothing, are how hidden text gets smuggled into a reply or a page.
func TestHiddenCharactersAreRevealed(t *testing.T) {
	for source, want := range map[string]string{
		"a\u200bb":                     `a\u200bb`,
		"a\u200db":                     `a\u200db`,
		"a\u00adb":                     `a\u00adb`,
		"abc\u202edef":                 `abc\u202edef`,
		"hi\ufe00\U000e0100":           `hi\ufe00\U000e0100`,
		"⚠\ufe0f\ufe0f":                `⚠\ufe0f`,
		"x\U000e0041\U000e0042":        `x\U000e0041\U000e0042`,
		"line\u2028separator\ufeffbom": `line\u2028separator\ufeffbom`,
	} {
		if shown := revealHiddenPlain(source); shown != want {
			t.Errorf("%q shown as %q, want %q", source, shown, want)
		}
	}
}

func TestStyledTextKeepsItsColours(t *testing.T) {
	line := styleUser.Render("You: ") + "tab\there \x1b[2Jgone"
	shown := revealHiddenStyled(line)
	if !strings.HasPrefix(shown, styleUser.Render("You: ")) {
		t.Errorf("%q lost its styling", shown)
	}
	if want := `tab        here \x1b[2Jgone`; !strings.HasSuffix(shown, want) {
		t.Errorf("%q does not end %q", shown, want)
	}
}

// What the renderer counts a row as and what a terminal drawing it code point
// by code point counts it as must be the same, or the row wraps on screen.
func TestMixedScriptsMeasureTheSameEitherWay(t *testing.T) {
	for _, line := range strings.Split(revealHiddenPlain(mixedScriptReply), "\n") {
		if grapheme, perCodePoint := ansi.StringWidth(line), ansi.StringWidthWc(line); grapheme != perCodePoint {
			t.Errorf("%q is %d cells to the renderer and %d drawn code point by code point", line, grapheme, perCodePoint)
		}
	}
}

// Hangul spelled out in jamo is the one cluster terminals do join, so it is
// left whole.
func TestHangulJamoStayJoined(t *testing.T) {
	jamo := "한글"
	if shown := revealHiddenPlain(jamo); shown != jamo {
		t.Errorf("%q shown as %q", jamo, shown)
	}
}

// Every code point, alone and in the places a combining character or an
// emoji modifier would join it to its neighbours, must leave nothing a
// terminal could draw wider than the renderer counts it. The other way round,
// a terminal drawing narrower, only leaves the cells at the end of the row.
// Planes 2 and 3 hold nothing but ideographs, and planes 15 and 16 nothing but
// private use, so the sweep leaves them out.
func TestNoCodePointDrawsWiderThanCounted(t *testing.T) {
	if testing.Short() {
		t.Skip("sweeps every code point")
	}
	for character := rune(0); character <= 0xe0fff; character++ {
		if character >= 0xd800 && character <= 0xdfff {
			continue
		}
		if character == 0x20000 {
			character = 0xe0000
		}
		single := string(character)
		for _, text := range []string{"a" + single, single + "\u0301", "👍" + single, "क" + single + "\u093e"} {
			shown := revealHiddenPlain(text)
			if perCodePoint, grapheme := ansi.StringWidthWc(shown), ansi.StringWidth(shown); perCodePoint > grapheme {
				t.Fatalf("%q shown as %q: %d cells drawn code point by code point, %d counted", text, shown, perCodePoint, grapheme)
			}
		}
	}
}

func TestCopyingTakesClusterBreaksOut(t *testing.T) {
	hindi := "नमस्ते दुनिया"
	shown := revealHiddenPlain(hindi)
	if !strings.Contains(shown, clusterBreak) {
		t.Fatalf("%q was not split", shown)
	}
	copied := selectedText([]string{shown}, nil, 0, selectionPoint{}, selectionPoint{column: ansi.StringWidth(shown)})
	if copied != hindi {
		t.Errorf("copied %q, want %q", copied, hindi)
	}
}

// Every row of the screen must be exactly as wide as the terminal under both
// counts, with nothing in it but text and colours, whatever the model, the
// tools or the operator put on it — or the renderer loses its place, leaving a
// doubled status line and the input box pushed off the bottom.
func TestEveryRowKeepsTheScreenWidth(t *testing.T) {
	const width, height = 170, 50
	m := &Model{input: newInputBox(width - 6)}
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m.appendLine(styleUser.Render("You: ") + "Decode\tthis " + mixedScriptReply)
	m.startTool("call-1", "shell_exec", `{"command": "cat .env.production\u000b"}`)
	m.finishTool("call-1", "shell_exec", binaryToolOutput, false)
	m.appendReply(mixedScriptReply)
	m.busy = true
	m.runningTool = "evil\x0b\u200dtool"
	m.pendingAs.WriteString(mixedScriptReply)
	m.input.SetValue("👨\u200d👩\u200d👧 नमस्ते दुनिया ｶﾞ")
	m.relayout()

	for _, expanded := range []bool{false, true} {
		m.transcript.entries[1].expanded = expanded
		m.transcript.refresh(m.transcript.entries[1])
		m.renderChat()
		rows := strings.Split(m.View(), "\n")
		if len(rows) != height {
			t.Fatalf("expanded=%v: %d rows, want %d", expanded, len(rows), height)
		}
		for index, row := range rows {
			if grapheme, perCodePoint := ansi.StringWidth(row), ansi.StringWidthWc(row); grapheme != width || perCodePoint != width {
				t.Errorf("expanded=%v row %d is %d cells counted, %d drawn: %q", expanded, index, grapheme, perCodePoint, ansi.Strip(row))
			}
			for _, character := range sgrSequence.ReplaceAllString(strings.ReplaceAll(row, "\x1b[m", ""), "") {
				if unicode.IsControl(character) {
					t.Errorf("expanded=%v row %d carries control character %U: %q", expanded, index, character, ansi.Strip(row))
					break
				}
			}
		}
	}
}
