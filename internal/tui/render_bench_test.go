package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/x/ansi"
)

// TestWrappedCacheMatchesFullWrap pins the invariant B1a relies on:
// incrementally wrapping each entry as it is appended must produce the same
// visible output as wrapping the concatenated body in one pass.
func TestWrappedCacheMatchesFullWrap(t *testing.T) {
	widths := []int{20, 40, 80, 120}
	entries := []string{
		styleUser.Render("You: ") + "hi",
		styleAssist.Render("hello, this is a longer assistant reply that will wrap at narrower widths and stay on one line at wide widths"),
		styleTool.Render("→ shell_exec ") + `{"command":"ls -la /workspace"}`,
		styleToolRes.Render("  ✓ total 8\ndrwxr-xr-x 2 root root 4096 Jan  1 00:00 .\ndrwxr-xr-x 3 root root 4096 Jan  1 00:00 .."),
		styleErr.Render("  ✗ command not found"),
		styleMuted.Render("↻ resumed previous session"),
		styleAssist.Render(strings.Repeat("streamed token ", 40)),
	}
	for _, w := range widths {
		m := benchModel(w)
		// Force wrap cache to initialise at this width without touching the
		// viewport's uninitialised state — appendLine only maintains the
		// cache once wrapWidth is set.
		m.rebuildWrapped(w)
		var raw strings.Builder
		for i, e := range entries {
			if i > 0 {
				raw.WriteString("\n")
			}
			raw.WriteString(e)
			m.appendLine(e)
		}
		got := m.wrappedBody.String()
		want := ansi.Wrap(raw.String(), w, "")
		if got != want {
			t.Fatalf("width=%d: incremental wrap diverged from one-shot wrap\n--- got\n%q\n--- want\n%q", w, got, want)
		}
	}
}

// benchModel returns a Model wired just enough to exercise appendLine /
// renderChat: a viewport with a fixed width and no runner/agent.
func benchModel(width int) *Model {
	vp := viewport.New(width, 20)
	vp.Width = width
	vp.Height = 20
	return &Model{
		chat:  vp,
		width: width + 32,
	}
}

// BenchmarkAppendLineWithHistory measures the per-line cost of appendLine
// after the transcript already holds `preload` lines. If the cost grows with
// preload, the render path is quadratic in transcript length.
func BenchmarkAppendLineWithHistory(b *testing.B) {
	line := styleAssist.Render(strings.Repeat("hello world ", 20))
	for _, preload := range []int{0, 100, 500, 1000} {
		b.Run(fmtInt(preload), func(b *testing.B) {
			m := benchModel(80)
			for i := 0; i < preload; i++ {
				m.appendLine(line)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.appendLine(line)
			}
		})
	}
}

// BenchmarkStreamReply simulates one streamed reply of `chunks` deltas after a
// pre-existing transcript, renderChat called once per delta. This is the
// pre-B1b behaviour and isolates the effect of the wrapped-body cache (B1a).
func BenchmarkStreamReply(b *testing.B) {
	line := styleAssist.Render(strings.Repeat("committed line ", 20))
	chunk := "some streaming token content "
	const chunks = 200
	for _, preload := range []int{0, 200, 500} {
		b.Run(fmtInt(preload), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				m := benchModel(80)
				for j := 0; j < preload; j++ {
					m.appendLine(line)
				}
				b.StartTimer()
				for j := 0; j < chunks; j++ {
					m.pendingAs.WriteString(chunk)
					m.renderChat()
				}
			}
		})
	}
}

// BenchmarkStreamReplyCoalesced simulates the post-B1b path: deltas accumulate
// into pendingAs but renderChat is only called once per batch, as it would be
// under the ~50ms coalescing tick. Compare to BenchmarkStreamReply to see the
// combined B1a + B1b saving.
func BenchmarkStreamReplyCoalesced(b *testing.B) {
	line := styleAssist.Render(strings.Repeat("committed line ", 20))
	chunk := "some streaming token content "
	const (
		chunks    = 200
		batchSize = 10 // one render per ~10 deltas, ≈ what a 50ms tick catches
	)
	for _, preload := range []int{0, 200, 500} {
		b.Run(fmtInt(preload), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				m := benchModel(80)
				for j := 0; j < preload; j++ {
					m.appendLine(line)
				}
				b.StartTimer()
				for j := 0; j < chunks; j++ {
					m.pendingAs.WriteString(chunk)
					if (j+1)%batchSize == 0 {
						m.renderChat()
					}
				}
				m.renderChat() // final flush, as AssistantDoneMsg would do
			}
		})
	}
}

func fmtInt(n int) string {
	if n == 0 {
		return "empty"
	}
	// tiny custom formatter to avoid pulling strconv into a benchmark helper
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return "preload=" + string(digits)
}
