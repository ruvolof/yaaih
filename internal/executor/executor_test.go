package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const testPromptMark = promptMarkPrefix + "0123456789abcdef0123456789abcdef__"

// feed pushes lines onto a channel the way a session's reader goroutine does,
// ending with a prompt sentinel carrying cwd.
func feed(lines []string, cwd string) chan string {
	ch := make(chan string, len(lines)+1)
	for _, line := range lines {
		ch <- line
	}
	ch <- testPromptMark + ":" + cwd + "\n"
	return ch
}

func readAll(t *testing.T, ch chan string, maxBytes int) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, cwd, discarded, err := readUntilPromptFrom(ctx, ch, testPromptMark, maxBytes)
	if err != nil {
		t.Fatalf("readUntilPromptFrom: %v", err)
	}
	return out, cwd, discarded
}

func TestReadUntilPromptReturnsOutputAndCWD(t *testing.T) {
	out, cwd, discarded := readAll(t, feed([]string{"alpha\n", "beta\n"}, "/workspace/sub"), maxCapturedBytes)

	if out != "alpha\nbeta\n" {
		t.Errorf("output = %q, want %q", out, "alpha\nbeta\n")
	}
	if cwd != "/workspace/sub" {
		t.Errorf("cwd = %q, want /workspace/sub", cwd)
	}
	if discarded != 0 {
		t.Errorf("discarded = %d, want 0", discarded)
	}
}

// The cap bounds what is retained, never what is consumed. If the loop stopped
// reading at the cap, the unread tail would still be sitting on the channel and
// the *next* command would read it as its own result.
func TestReadUntilPromptDrainsPastTheCap(t *testing.T) {
	line := strings.Repeat("x", 100) + "\n"
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = line
	}
	ch := feed(lines, "/workspace")

	out, cwd, discarded := readAll(t, ch, 1000)

	if len(out) > 1000 {
		t.Errorf("retained %d bytes, want at most the 1000-byte cap", len(out))
	}
	if discarded == 0 {
		t.Error("discarded = 0, want the dropped lines accounted for")
	}
	if len(out)+discarded != 500*len(line) {
		t.Errorf("retained %d + discarded %d != produced %d", len(out), discarded, 500*len(line))
	}
	if cwd != "/workspace" {
		t.Errorf("cwd = %q, want the sentinel to have been reached", cwd)
	}
	if len(ch) != 0 {
		t.Errorf("%d lines left on the channel; the next command would read them", len(ch))
	}
}

// Dropping whole lines keeps the retained text valid UTF-8, which a byte-exact
// cut through a multi-byte rune would not.
func TestReadUntilPromptDropsWholeLines(t *testing.T) {
	lines := []string{"keep this\n", strings.Repeat("é", 400) + "\n", "and this\n"}
	out, _, discarded := readAll(t, feed(lines, "/workspace"), 64)

	if !strings.HasPrefix(out, "keep this\n") {
		t.Errorf("output = %q, want it to start with the line that fit", out)
	}
	if strings.Contains(out, "é") {
		t.Errorf("output = %q, want the oversized line dropped whole", out)
	}
	if discarded != len(strings.Repeat("é", 400)+"\n") {
		t.Errorf("discarded = %d, want the whole oversized line", discarded)
	}
}

func TestReadUntilPromptUncapped(t *testing.T) {
	lines := []string{strings.Repeat("y", 5000) + "\n"}
	out, _, discarded := readAll(t, feed(lines, "/workspace"), 0)

	if len(out) != 5001 {
		t.Errorf("len(output) = %d, want 5001 with the cap disabled", len(out))
	}
	if discarded != 0 {
		t.Errorf("discarded = %d, want 0", discarded)
	}
}

func TestReadUntilPromptStopsAtSentinelMidLine(t *testing.T) {
	ch := make(chan string, 2)
	ch <- "trailing output" + testPromptMark + ":/workspace\n"
	out, cwd, _ := readAll(t, ch, maxCapturedBytes)

	if out != "trailing output" {
		t.Errorf("output = %q, want %q", out, "trailing output")
	}
	if cwd != "/workspace" {
		t.Errorf("cwd = %q, want /workspace", cwd)
	}
}

// The reader closes the channel when bash's output ends. What bash wrote before
// it went away is still the command's output, and the end has to come back as
// the shell exiting rather than as a wait for a sentinel that will never come.
func TestReadUntilPromptReportsTheShellExiting(t *testing.T) {
	ch := make(chan string, 2)
	ch <- "last words\n"
	close(ch)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _, _, err := readUntilPromptFrom(ctx, ch, testPromptMark, maxCapturedBytes)

	if !errors.Is(err, errShellExited) {
		t.Errorf("err = %v, want errShellExited", err)
	}
	if out != "last words\n" {
		t.Errorf("output = %q, want what arrived before the end", out)
	}
}

// A log field cut mid-rune would hold an invalid byte sequence.
func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		input string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"abcdef", 3, "abc…"},
		{"aé", 2, "a…"},
		{"日本語", 4, "日…"},
		{"日本語", 6, "日本…"},
		{"🙂🙂", 3, "…"},
	} {
		got := truncate(tc.input, tc.limit)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.limit, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q, not valid UTF-8", tc.input, tc.limit, got)
		}
	}
}

// Output is arbitrary text, so it can contain something shaped like a sentinel:
// the marker this package once used as a constant, or another session's. Only
// this session's marker may end the read.
func TestReadUntilPromptIgnoresOtherMarkers(t *testing.T) {
	lines := []string{
		"before\n",
		"__HARNESS_PROMPT__:/elsewhere\n",
		promptMarkPrefix + "ffffffffffffffffffffffffffffffff__:/elsewhere\n",
		"after\n",
	}
	out, cwd, _ := readAll(t, feed(lines, "/workspace"), maxCapturedBytes)

	if want := strings.Join(lines, ""); out != want {
		t.Errorf("output = %q, want every line up to this session's sentinel: %q", out, want)
	}
	if cwd != "/workspace" {
		t.Errorf("cwd = %q, want /workspace from this session's sentinel", cwd)
	}
}

func TestPromptMarksAreUniqueAndGreppable(t *testing.T) {
	first, err := newPromptMark()
	if err != nil {
		t.Fatalf("newPromptMark: %v", err)
	}
	second, err := newPromptMark()
	if err != nil {
		t.Fatalf("newPromptMark: %v", err)
	}
	if first == second {
		t.Errorf("two marks are both %q, want each session to draw its own", first)
	}
	for _, promptMark := range []string{first, second} {
		if !strings.HasPrefix(promptMark, promptMarkPrefix) || !strings.HasSuffix(promptMark, "__") {
			t.Errorf("mark %q, want it shaped %s<nonce>__", promptMark, promptMarkPrefix)
		}
		if strings.ContainsAny(promptMark, `'"\%:`) {
			t.Errorf("mark %q holds a character that would break the sentinel printf or its parsing", promptMark)
		}
	}
}
