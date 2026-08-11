package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// feed pushes lines onto a channel the way a session's reader goroutine does,
// ending with a prompt sentinel carrying cwd.
func feed(lines []string, cwd string) chan readResult {
	ch := make(chan readResult, len(lines)+1)
	for _, line := range lines {
		ch <- readResult{line: line}
	}
	ch <- readResult{line: promptMark + ":" + cwd + "\n"}
	return ch
}

func readAll(t *testing.T, ch chan readResult, maxBytes int) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, cwd, discarded, err := readUntilPromptFrom(ctx, ch, maxBytes)
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
	ch := make(chan readResult, 2)
	ch <- readResult{line: "trailing output" + promptMark + ":/workspace\n"}
	out, cwd, _ := readAll(t, ch, maxCapturedBytes)

	if out != "trailing output" {
		t.Errorf("output = %q, want %q", out, "trailing output")
	}
	if cwd != "/workspace" {
		t.Errorf("cwd = %q, want /workspace", cwd)
	}
}
