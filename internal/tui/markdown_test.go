package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/ruvolof/yaaih/internal/agent"
	"github.com/ruvolof/yaaih/internal/llm"
)

const markdownReply = "Run **this** first:\n\n```bash\nls -la /workspace\n```\n\n- one\n- two"

// A reply streams in raw — rendering partial Markdown per chunk would re-lay
// it out on every delta — and is drawn as Markdown once the message is done.
func TestReplyIsRenderedAsMarkdownOnceCommitted(t *testing.T) {
	m := sizedTestModel(t)
	m.Update(agent.AssistantDeltaMsg{Text: markdownReply})
	m.renderChat()

	if streaming := visibleChat(m); !strings.Contains(streaming, "**this**") {
		t.Fatalf("the in-flight reply is not shown as streamed:\n%s", streaming)
	}

	m.Update(agent.AssistantMessageMsg{})

	committed := visibleChat(m)
	for _, markup := range []string{"**", "```", "- one"} {
		if strings.Contains(committed, markup) {
			t.Errorf("committed reply still shows %q:\n%s", markup, committed)
		}
	}
	for _, text := range []string{"Run this first:", "ls -la /workspace", "• one", "• two"} {
		if !strings.Contains(committed, text) {
			t.Errorf("committed reply lost %q:\n%s", text, committed)
		}
	}
}

// Copying the transcript or the last reply gives the Markdown the model wrote,
// not the way it was drawn.
func TestCopiedReplyIsItsMarkdownSource(t *testing.T) {
	m := sizedTestModel(t)
	m.Update(agent.AssistantDeltaMsg{Text: markdownReply})
	m.Update(agent.AssistantMessageMsg{})

	if plain := m.transcript.plainText(); plain != markdownReply {
		t.Errorf("plainText = %q, want the source %q", plain, markdownReply)
	}
	if m.lastAssistant != markdownReply {
		t.Errorf("lastAssistant = %q, want the source", m.lastAssistant)
	}
}

// Glamour pads every line to the wrap width; the transcript must still fit
// the viewport, code included, and not carry the padding.
func TestRenderedReplyFitsTheWidth(t *testing.T) {
	var narrow transcript
	narrow.setWidth(30)
	entry := narrow.appendReply("```\n" + strings.Repeat("x", 80) + "\n```\n\n" + strings.Repeat("word ", 30))

	for _, line := range strings.Split(entry.layout, "\n") {
		if width := ansi.StringWidth(line); width > 30 {
			t.Errorf("line is %d wide, past 30: %q", width, line)
		}
		if strings.HasSuffix(line, " ") {
			t.Errorf("line keeps glamour's padding: %q", line)
		}
	}
	if strings.HasPrefix(entry.layout, "\n") || strings.HasSuffix(entry.layout, "\n") {
		t.Errorf("layout keeps glamour's blank lines: %q", entry.layout)
	}
}

// A resumed conversation draws its replies the way the live loop did.
func TestResumedReplyIsRenderedAsMarkdown(t *testing.T) {
	m := sizedTestModel(t)
	m.prefillFromHistory([]llm.Message{
		{Role: llm.RoleUser, Content: "what now?"},
		{Role: llm.RoleAssistant, Content: markdownReply},
	})

	shown := visibleChat(m)
	if strings.Contains(shown, "**this**") || !strings.Contains(shown, "Run this first:") {
		t.Errorf("resumed reply not rendered as Markdown:\n%s", shown)
	}
	if m.lastAssistant != markdownReply {
		t.Errorf("lastAssistant = %q, want the resumed reply", m.lastAssistant)
	}
}

func modelWithReplies(t *testing.T, count int) *Model {
	t.Helper()
	m := sizedTestModel(t)
	for index := range count {
		paragraph := strings.Repeat("prose that wraps differently at every width ", 8)
		m.Update(agent.AssistantDeltaMsg{Text: fmt.Sprintf("Reply %d: %s\n\n%s", index, markdownReply, paragraph)})
		m.Update(agent.AssistantMessageMsg{})
	}
	return m
}

// catchUp delivers reply-render ticks until no stand-in is left.
func catchUp(t *testing.T, m *Model) (ticks int) {
	t.Helper()
	for m.transcript.hasStandIns() {
		if ticks++; ticks > 1000 {
			t.Fatal("stand-ins never all rendered")
		}
		m.Update(replyRenderTickMsg{})
	}
	return ticks
}

func standIns(m *Model) (count int) {
	for _, entry := range m.transcript.entries {
		if m.transcript.showsStandIn(entry) {
			count++
		}
	}
	return count
}

// A resize renders only what is on screen before the next frame; the replies
// out of view are drawn over the following ticks and end up exactly as if the
// session had been at the new width all along.
func TestResizeRendersRepliesInViewFirst(t *testing.T) {
	const replies = 60
	m := modelWithReplies(t, replies)

	if _, cmd := m.Update(tea.WindowSizeMsg{Width: 90, Height: 40}); cmd == nil {
		t.Fatal("a resize that left stand-ins scheduled nothing to draw them")
	}
	if left := standIns(m); left == 0 || left == replies {
		t.Fatalf("stand-ins after resize = %d, want the on-screen replies drawn and the rest not", left)
	}
	if shown := visibleChat(m); strings.Contains(shown, "**this**") {
		t.Errorf("a stand-in is on screen:\n%s", shown)
	}

	catchUp(t, m)

	var fresh transcript
	fresh.setWidth(m.chat.Width)
	for _, entry := range m.transcript.entries {
		fresh.appendReply(entry.reply)
	}
	if m.transcript.body.String() != fresh.body.String() {
		t.Error("after catching up, the transcript differs from one laid out at the new width")
	}
}

// Drawing replies again after a resize must not move the text the operator
// scrolled back to.
func TestResizeKeepsTheReaderInPlace(t *testing.T) {
	m := modelWithReplies(t, 60)
	reading := m.transcript.entries[20]
	m.chat.SetYOffset(reading.firstLine + 1)
	m.userScrolled = true

	m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	catchUp(t, m)

	if top := m.transcript.entryAtLine(m.chat.YOffset); top != reading {
		t.Errorf("view starts in entry %d, want %d", top.index, reading.index)
	}
	if m.chat.YOffset != reading.firstLine+1 {
		t.Errorf("view starts %d lines into the entry, want 1", m.chat.YOffset-reading.firstLine)
	}
}
