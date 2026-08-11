// Package toolout reduces tool output before it enters the conversation
// history.
//
// Reduction runs in two stages with deliberately different guarantees.
// Normalize strips terminal control noise — escape sequences, carriage-return
// repaints, runs of identical lines — none of which a reader ever sees as
// distinct information, so it runs unconditionally. Truncate drops content, so
// it runs only when a byte budget demands it and always says so in the output
// it returns.
//
// Everything here is pure string work: callers decide the budgets and what to
// do with the statistics.
package toolout

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// Stats reports how many bytes each stage removed. The per-stage fields are
// measured against that stage's own input, so they sum to OriginalBytes minus
// FinalBytes.
type Stats struct {
	OriginalBytes int
	FinalBytes    int

	ANSIBytes      int
	ProgressBytes  int
	RepeatBytes    int
	TruncatedBytes int
}

// Removed is the total number of bytes dropped.
func (s Stats) Removed() int { return s.OriginalBytes - s.FinalBytes }

// Reduce runs Normalize and then Truncate against budget, returning the
// combined statistics. note is passed through to Truncate.
func Reduce(text string, budget int, note string) (string, Stats) {
	normalized, stats := Normalize(text)
	truncated, cut := Truncate(normalized, budget, note)
	stats.TruncatedBytes = cut.TruncatedBytes
	stats.FinalBytes = cut.FinalBytes
	return truncated, stats
}

// Normalize removes terminal control noise. What survives is what someone
// watching the terminal would have ended up seeing, which is also what the
// model needs — and it is a fraction of the bytes.
func Normalize(text string) (string, Stats) {
	stats := Stats{OriginalBytes: len(text)}

	stripped := ansi.Strip(text)
	stats.ANSIBytes = len(text) - len(stripped)

	repainted := collapseProgressRepaints(stripped)
	stats.ProgressBytes = len(stripped) - len(repainted)

	deduped := collapseRepeatedLines(repainted)
	stats.RepeatBytes = len(repainted) - len(deduped)

	stats.FinalBytes = len(deduped)
	return deduped, stats
}

// Truncate bounds text to budget bytes by keeping its head and its tail and
// describing what it dropped in between. note, when non-empty, is added to that
// description on its own line — the caller uses it to say where the full output
// can still be found. A budget of zero or less disables truncation.
//
// The returned string slightly exceeds budget by the length of the description,
// which is bounded and small.
func Truncate(text string, budget int, note string) (string, Stats) {
	stats := Stats{OriginalBytes: len(text), FinalBytes: len(text)}
	if budget <= 0 || len(text) <= budget {
		return text, stats
	}

	headBudget := budget / 2
	head := text[:cutBefore(text, headBudget)]
	tail := text[cutAfter(text, len(text)-(budget-headBudget)):]
	dropped := len(text) - len(head) - len(tail)

	var marker strings.Builder
	fmt.Fprintf(&marker, "\n\n…[output truncated: %s total, kept the first %s and the last %s, dropped %s]",
		HumanBytes(len(text)), HumanBytes(len(head)), HumanBytes(len(tail)), HumanBytes(dropped))
	if note != "" {
		marker.WriteString("\n[" + note + "]")
	}
	marker.WriteString("\n\n")

	out := head + marker.String() + tail
	stats.TruncatedBytes = dropped
	stats.FinalBytes = len(out)
	return out, stats
}

// HumanBytes renders a byte count for a marker the model will read.
func HumanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// collapseProgressRepaints keeps only the final state of each carriage-return
// repaint. Progress bars — pip, npm, docker pull, curl, wget — rewrite a single
// line hundreds or thousands of times, and every state but the last was already
// invisible on the terminal that produced it.
func collapseProgressRepaints(text string) string {
	if !strings.ContainsRune(text, '\r') {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	remaining := text
	for {
		line, rest, more := strings.Cut(remaining, "\n")
		out.WriteString(lastRepaint(line))
		if !more {
			return out.String()
		}
		out.WriteByte('\n')
		remaining = rest
	}
}

// lastRepaint returns the final visible state of a line that may carry carriage
// returns. A trailing \r parks the cursor at the start of a line nothing else
// writes to, so the last non-empty segment is what remains on screen. This also
// absorbs CRLF line endings, whose \r is the degenerate case.
func lastRepaint(line string) string {
	if !strings.ContainsRune(line, '\r') {
		return line
	}
	segments := strings.Split(line, "\r")
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i] != "" {
			return segments[i]
		}
	}
	return ""
}

// minRepeatRun is the number of identical consecutive lines at which the run is
// replaced by a count. Two in a row is ordinary output and a marker would cost
// more than it saves.
const minRepeatRun = 3

func collapseRepeatedLines(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		run := 1
		for i+run < len(lines) && lines[i+run] == lines[i] {
			run++
		}
		out = append(out, lines[i])
		switch {
		case run < minRepeatRun:
			for repeat := 1; repeat < run; repeat++ {
				out = append(out, lines[i])
			}
		case lines[i] == "":
			out = append(out, fmt.Sprintf("[%d blank lines omitted]", run-1))
		default:
			out = append(out, fmt.Sprintf("[previous line repeated %d more times]", run-1))
		}
		i += run
	}
	return strings.Join(out, "\n")
}

// lineSearchWindow is how far a cut may move to land on a line boundary instead
// of mid-line. Past this it costs more budget than the tidiness is worth.
const lineSearchWindow = 256

// cutBefore returns the largest offset at or below limit that is safe to cut
// at: a line boundary when one is close, otherwise a rune boundary.
func cutBefore(text string, limit int) int {
	if limit >= len(text) {
		return len(text)
	}
	if nl := strings.LastIndexByte(text[:limit], '\n'); nl >= 0 && limit-nl <= lineSearchWindow {
		return nl + 1
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return limit
}

// cutAfter is cutBefore's mirror: the smallest offset at or above start that is
// safe to resume from.
func cutAfter(text string, start int) int {
	if start <= 0 {
		return 0
	}
	window := start + lineSearchWindow
	if window > len(text) {
		window = len(text)
	}
	if nl := strings.IndexByte(text[start:window], '\n'); nl >= 0 {
		return start + nl + 1
	}
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return start
}
