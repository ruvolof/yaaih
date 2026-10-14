package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/ruvolof/yaaih/internal/agent"
)

// mouseAt sends a left-button event over the chat cell that shows point, as
// the view is scrolled now.
func mouseAt(m *Model, point selectionPoint, action tea.MouseAction) {
	left, top := chatContentOrigin()
	m.Update(tea.MouseMsg{
		X: left + point.column, Y: top + point.line - m.chat.YOffset,
		Action: action, Button: tea.MouseButtonLeft,
	})
}

func clickChat(m *Model, point selectionPoint) {
	mouseAt(m, point, tea.MouseActionPress)
	mouseAt(m, point, tea.MouseActionRelease)
}

func dragChat(m *Model, from, to selectionPoint) {
	mouseAt(m, from, tea.MouseActionPress)
	mouseAt(m, to, tea.MouseActionMotion)
	mouseAt(m, to, tea.MouseActionRelease)
}

// captureClipboard swaps the system clipboard for a string for the test.
func captureClipboard(t *testing.T) *string {
	t.Helper()
	var copied string
	previous := writeClipboard
	writeClipboard = func(text string) error {
		copied = text
		return nil
	}
	t.Cleanup(func() { writeClipboard = previous })
	return &copied
}

func TestOSC52EncodesTextForTheTerminal(t *testing.T) {
	got := osc52("héllo\nworld")
	want := "\x1b]52;c;aMOpbGxvCndvcmxk\a"
	if got != want {
		t.Errorf("osc52 = %q, want %q", got, want)
	}
}

// lineOf is the first chat line whose text contains needle.
func lineOf(t *testing.T, m *Model, needle string) int {
	t.Helper()
	lines, _ := m.chatLines(0, m.chat.TotalLineCount())
	for number, line := range lines {
		if strings.Contains(ansi.Strip(line), needle) {
			return number
		}
	}
	t.Fatalf("%q is not in the chat", needle)
	return 0
}

func TestSelectedTextReadsAsDrawnWithoutStyling(t *testing.T) {
	lines := []string{
		styleUser.Render("You: ") + "first line   ",
		styleTool.Render("▸ shell_exec") + " second",
		"日本語 third",
	}
	start := selectionPoint{line: 10, column: 5}
	end := selectionPoint{line: 12, column: 6}

	got := selectedText(lines, nil, 10, start, end)

	want := "first line\n▸ shell_exec second\n日本語"
	if got != want {
		t.Errorf("selectedText = %q, want %q", got, want)
	}
}

func TestSelectedTextJoinsWrapsAndSkipsGutters(t *testing.T) {
	lines := []string{"    1 │ echo one", "      │ two", "    2 │ next", "word", "wrapped"}
	info := []lineInfo{
		{gutter: 8},
		{gutter: 8, continues: true},
		{gutter: 8},
		{},
		{continues: true, joiner: " "},
	}

	got := selectedText(lines, info, 0, selectionPoint{line: 0, column: 0}, selectionPoint{line: 4, column: 100})

	want := "echo onetwo\nnext\nword wrapped"
	if got != want {
		t.Errorf("selectedText = %q, want %q", got, want)
	}
}

func TestWrapEachRecordsWhatEachBreakTookOut(t *testing.T) {
	wordWrapped, wordInfo := wrapText("alpha bravo charlie\nshort", 11)
	if wordWrapped != "alpha bravo\ncharlie\nshort" {
		t.Fatalf("wrapped to %q", wordWrapped)
	}
	wantWord := []lineInfo{{}, {continues: true, joiner: " "}, {}}
	if !slices.Equal(wordInfo, wantWord) {
		t.Errorf("word wrap info = %+v, want %+v", wordInfo, wantWord)
	}

	cut, cutInfo := wrapEach("abcdefgh", func(line string) string { return ansi.Hardwrap(line, 3, true) })
	if len(cut) != 3 || cutInfo[1] != (lineInfo{continues: true}) || cutInfo[2] != (lineInfo{continues: true}) {
		t.Errorf("hard wrap gave %q with %+v", cut, cutInfo)
	}

	multiline := styleAssist.Render("first line that wraps somewhere\n\nthird line, also long enough to wrap")
	if tracked, _ := wrapText(multiline, 12); tracked != ansi.Wrap(multiline, 12, "") {
		t.Errorf("wrapping a line at a time differs from wrapping the whole:\n%q\n%q", tracked, ansi.Wrap(multiline, 12, ""))
	}

	_, unbroken := wrapText("fits", 80)
	if unbroken != nil {
		t.Errorf("an unbroken text carries info %+v", unbroken)
	}
}

func TestSelectionSpansTheCellsUnderBothEnds(t *testing.T) {
	backwards := textSelection{anchor: selectionPoint{line: 3, column: 4}, head: selectionPoint{line: 1, column: 2}}
	start, end := backwards.span()
	if start != (selectionPoint{line: 1, column: 2}) || end != (selectionPoint{line: 3, column: 5}) {
		t.Errorf("span = %v, %v", start, end)
	}
}

func TestHighlightKeepsEveryRowItsWidth(t *testing.T) {
	rows := []string{
		styleErr.Render("error: something") + "    ",
		"plain row padded    ",
		"日本語 row          ",
	}
	widths := make([]int, len(rows))
	for index, row := range rows {
		widths[index] = ansi.StringWidth(row)
	}

	highlightRows(rows, nil, 0, selectionPoint{line: 0, column: 3}, selectionPoint{line: 2, column: 4})

	for index, row := range rows {
		if ansi.StringWidth(row) != widths[index] {
			t.Errorf("row %d is %d cells wide after highlighting, was %d", index, ansi.StringWidth(row), widths[index])
		}
	}
	if ansi.Strip(rows[2]) != "日本語 row          " {
		t.Errorf("highlighting changed the text: %q", ansi.Strip(rows[2]))
	}
}

func TestDraggingSelectsAndCtrlYCopiesIt(t *testing.T) {
	copied := captureClipboard(t)
	m := sizedTestModel(t)
	m.appendLine("alpha bravo charlie")
	m.appendLine("delta echo foxtrot")
	first := lineOf(t, m, "alpha")

	dragChat(m, selectionPoint{line: first, column: 6}, selectionPoint{line: first + 1, column: 9})
	pressKey(m, tea.KeyCtrlY)

	if *copied != "bravo charlie\ndelta echo" {
		t.Errorf("copied %q", *copied)
	}
}

func TestCtrlYWithoutASelectionCopiesTheLastReply(t *testing.T) {
	copied := captureClipboard(t)
	m := sizedTestModel(t)
	m.appendLine("some narration")
	m.lastAssistant = "**the reply**"

	clickChat(m, selectionPoint{line: lineOf(t, m, "narration"), column: 2})
	pressKey(m, tea.KeyCtrlY)

	if *copied != "**the reply**" {
		t.Errorf("copied %q", *copied)
	}
}

func TestSelectionCopiesTheReplyAsDrawn(t *testing.T) {
	copied := captureClipboard(t)
	m := sizedTestModel(t)
	m.appendReply("Steps:\n\n- **first** item\n- second item")
	first := lineOf(t, m, "first item")

	dragChat(m, selectionPoint{line: first, column: 0}, selectionPoint{line: first + 1, column: 100})
	pressKey(m, tea.KeyCtrlY)

	if strings.Contains(*copied, "**") || strings.Contains(*copied, "\x1b") {
		t.Errorf("copied markup or styling: %q", *copied)
	}
	if !strings.Contains(*copied, "first item") || !strings.Contains(*copied, "second item") {
		t.Errorf("copied %q", *copied)
	}
}

func TestSelectionReachesTheReplyStillStreaming(t *testing.T) {
	copied := captureClipboard(t)
	m := sizedTestModel(t)
	m.appendLine("committed line")
	m.Update(agent.AssistantDeltaMsg{Text: "streaming words"})
	m.Update(renderTickMsg{})
	line := lineOf(t, m, "streaming")

	dragChat(m, selectionPoint{line: line, column: 0}, selectionPoint{line: line, column: 8})
	pressKey(m, tea.KeyCtrlY)

	if *copied != "streaming" {
		t.Errorf("copied %q", *copied)
	}
}

func TestAnyOtherKeyDropsTheSelection(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine("alpha bravo charlie")
	line := lineOf(t, m, "alpha")
	dragChat(m, selectionPoint{line: line, column: 0}, selectionPoint{line: line, column: 4})

	pressKey(m, tea.KeyPgUp)
	if m.selection == nil {
		t.Fatal("scrolling dropped the selection")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.selection != nil {
		t.Error("typing kept the selection")
	}
}

func TestDraggingOverACallDoesNotToggleIt(t *testing.T) {
	m := sizedTestModel(t)
	runTool(m, "call-1", `{"command":"ls"}`, "a\nb")
	entry := lastToolEntry(m)

	dragChat(m, selectionPoint{line: entry.firstLine, column: 0}, selectionPoint{line: entry.firstLine, column: 6})

	if entry.expanded {
		t.Error("a drag across the call expanded it")
	}
	if m.selection == nil {
		t.Error("the drag selected nothing")
	}
}

func TestRelayingOutTheSelectedTextDropsTheSelection(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine("narration")
	runTool(m, "call-1", `{"command":"ls"}`, "a\nb")
	line := lineOf(t, m, "narration")

	dragChat(m, selectionPoint{line: line, column: 0}, selectionPoint{line: line + 1, column: 4})
	m.toggleTool(lastToolEntry(m))
	if m.selection != nil {
		t.Error("expanding a selected call kept the selection")
	}

	dragChat(m, selectionPoint{line: line, column: 0}, selectionPoint{line: line, column: 4})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.selection != nil {
		t.Error("a resize kept the selection")
	}
}

func TestNewContentBelowKeepsTheSelection(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine("narration")
	line := lineOf(t, m, "narration")
	dragChat(m, selectionPoint{line: line, column: 0}, selectionPoint{line: line, column: 4})

	runTool(m, "call-1", `{"command":"ls"}`, "a\nb")

	if m.selection == nil {
		t.Error("a call arriving below the selection dropped it")
	}
}

func TestTheViewHoldsStillWhileDragging(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine(numberedLines(60))
	m.Update(agent.AssistantDeltaMsg{Text: "streaming"})
	m.Update(renderTickMsg{})
	line := lineOf(t, m, "output line 050")
	mouseAt(m, selectionPoint{line: line, column: 0}, tea.MouseActionPress)
	offset := m.chat.YOffset

	m.Update(agent.AssistantDeltaMsg{Text: "\n" + numberedLines(10)})
	m.Update(renderTickMsg{})

	if m.chat.YOffset != offset {
		t.Errorf("the view moved from line %d to %d during the drag", offset, m.chat.YOffset)
	}
}

func TestDraggingPastTheTopEdgeScrolls(t *testing.T) {
	m := sizedTestModel(t)
	m.appendLine(numberedLines(100))
	start := selectionPoint{line: m.chat.YOffset + 2, column: 0}
	mouseAt(m, start, tea.MouseActionPress)
	offset := m.chat.YOffset

	left, top := chatContentOrigin()
	m.Update(tea.MouseMsg{X: left, Y: top - 1, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})

	if m.chat.YOffset != offset-1 {
		t.Errorf("YOffset = %d, want %d", m.chat.YOffset, offset-1)
	}
	if m.selection.head.line != offset-1 {
		t.Errorf("head on line %d, want %d", m.selection.head.line, offset-1)
	}
}

func TestTruncateCutsOnCharacters(t *testing.T) {
	got := truncate("caffè ☕ latte", 6)
	if got != "caffè …" {
		t.Errorf("truncate = %q", got)
	}
}

func TestCopyingAWrappedMessageJoinsItBack(t *testing.T) {
	copied := captureClipboard(t)
	m := sizedTestModel(t)
	message := strings.Repeat("lorem ipsum ", 20) + "end"
	m.appendLine(message)
	first := lineOf(t, m, "lorem")

	dragChat(m, selectionPoint{line: first, column: 0}, selectionPoint{line: first + 2, column: 100})
	pressKey(m, tea.KeyCtrlY)

	if *copied != message {
		t.Errorf("copied %q, want %q", *copied, message)
	}
}

func TestCopyingAnExpandedCallLeavesOutItsGutters(t *testing.T) {
	copied := captureClipboard(t)
	m := sizedTestModel(t)
	longLine := strings.Repeat("x", 150)
	runTool(m, "call-1", `{"command":"ls -la"}`, "first output\n"+longLine)
	entry := lastToolEntry(m)
	m.toggleTool(entry)

	dragChat(m, selectionPoint{line: entry.firstLine + 1, column: 0},
		selectionPoint{line: entry.firstLine + entry.lineCount - 1, column: 200})
	pressKey(m, tea.KeyCtrlY)

	want := "command\nls -la\n  ✓ 2 lines\nfirst output\n" + longLine
	if *copied != want {
		t.Errorf("copied %q, want %q", *copied, want)
	}
}

func TestApprovalArgumentsCanBeSelected(t *testing.T) {
	copied := captureClipboard(t)
	m, _ := approvalTestModel(t, "echo one\n"+strings.Repeat("y", 150))
	area, ok := m.paneArea(approvalPane)
	if !ok {
		t.Fatal("the panel shows no arguments to select")
	}

	m.Update(tea.MouseMsg{X: area.left, Y: area.top + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: area.left + area.width, Y: area.top + area.height - 1, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: area.left + area.width, Y: area.top + area.height - 1, Action: tea.MouseActionRelease})
	if !strings.Contains(m.renderApproval(), ansi.ResetStyle+styleSelection.Render("echo one")) {
		t.Error("the selection is not highlighted in the panel")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})

	want := "echo one\n" + strings.Repeat("y", 150)
	if *copied != want {
		t.Errorf("copied %q, want %q", *copied, want)
	}
	if m.pendingApproval == nil {
		t.Error("copying answered the approval")
	}
}

func TestInputTextCanBeSelected(t *testing.T) {
	copied := captureClipboard(t)
	m := sizedTestModel(t)
	typed := strings.Repeat("typed words ", 15) + "end"
	m.setInput(typed)
	area, ok := m.paneArea(inputPane)
	if !ok || area.height < 2 {
		t.Fatalf("the input box shows %d rows to select", area.height)
	}

	m.Update(tea.MouseMsg{X: area.left, Y: area.top, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: area.left + area.width, Y: area.top + area.height - 1, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: area.left + area.width, Y: area.top + area.height - 1, Action: tea.MouseActionRelease})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})

	if *copied != typed {
		t.Errorf("copied %q, want %q", *copied, typed)
	}
	if m.input.Value() != typed {
		t.Errorf("selecting changed the input to %q", m.input.Value())
	}
}
