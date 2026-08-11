package toolout

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeLeavesOrdinaryOutputAlone(t *testing.T) {
	text := "total 12\ndrwxr-xr-x 2 root root 4096 Jan  1 00:00 .\n-rw-r--r-- 1 root root   42 Jan  1 00:00 notes.txt\n"
	got, stats := Normalize(text)
	if got != text {
		t.Errorf("Normalize rewrote clean output:\n got %q\nwant %q", got, text)
	}
	if stats.Removed() != 0 {
		t.Errorf("Removed() = %d, want 0", stats.Removed())
	}
}

func TestNormalizeStripsANSI(t *testing.T) {
	text := "\x1b[0m\x1b[01;34mbin\x1b[0m  \x1b[01;32mrun.sh\x1b[0m\n"
	got, stats := Normalize(text)
	if got != "bin  run.sh\n" {
		t.Errorf("Normalize = %q, want %q", got, "bin  run.sh\n")
	}
	if stats.ANSIBytes == 0 {
		t.Error("ANSIBytes = 0, want the escape sequences accounted for")
	}
}

// A progress bar is one line rewritten many times. Only the final state was
// ever visible, so only the final state should reach the model.
func TestNormalizeCollapsesProgressRepaints(t *testing.T) {
	var bar strings.Builder
	for percent := 0; percent <= 100; percent++ {
		bar.WriteString("\rDownloading... ")
		bar.WriteString(strings.Repeat("#", percent/5))
		bar.WriteString(" ")
		bar.WriteString(strings.Repeat(" ", 20-percent/5))
	}
	text := "Fetching package\n" + bar.String() + "\nDone\n"

	got, stats := Normalize(text)

	if strings.Count(got, "Downloading") != 1 {
		t.Errorf("kept %d repaints, want 1:\n%q", strings.Count(got, "Downloading"), got)
	}
	if !strings.Contains(got, "####################") {
		t.Errorf("dropped the final state of the bar:\n%q", got)
	}
	if !strings.Contains(got, "Fetching package") || !strings.Contains(got, "Done") {
		t.Errorf("lost surrounding output:\n%q", got)
	}
	if stats.ProgressBytes < len(text)/2 {
		t.Errorf("ProgressBytes = %d, want most of the %d input bytes", stats.ProgressBytes, len(text))
	}
}

func TestNormalizeAbsorbsCRLF(t *testing.T) {
	got, _ := Normalize("alpha\r\nbeta\r\n")
	if got != "alpha\nbeta\n" {
		t.Errorf("Normalize = %q, want CRLF reduced to LF", got)
	}
}

func TestNormalizeCollapsesRepeatedLines(t *testing.T) {
	text := "start\n" + strings.Repeat("connection refused, retrying\n", 500) + "gave up\n"
	got, stats := Normalize(text)

	if strings.Count(got, "connection refused") != 1 {
		t.Errorf("kept %d copies, want 1", strings.Count(got, "connection refused"))
	}
	if !strings.Contains(got, "[previous line repeated 499 more times]") {
		t.Errorf("missing or miscounted repeat marker:\n%q", got)
	}
	if !strings.Contains(got, "start") || !strings.Contains(got, "gave up") {
		t.Errorf("lost surrounding output:\n%q", got)
	}
	if stats.RepeatBytes == 0 {
		t.Error("RepeatBytes = 0, want the collapsed run accounted for")
	}
}

// Two identical lines are ordinary output; a marker would be longer than the
// line it replaced.
func TestNormalizeKeepsShortRepeatRuns(t *testing.T) {
	text := "same\nsame\ndifferent\n"
	got, _ := Normalize(text)
	if got != text {
		t.Errorf("Normalize = %q, want %q", got, text)
	}
}

func TestNormalizeCollapsesBlankRuns(t *testing.T) {
	text := "head\n" + strings.Repeat("\n", 40) + "tail\n"
	got, _ := Normalize(text)
	if !strings.Contains(got, "blank lines omitted") {
		t.Errorf("blank run survived:\n%q", got)
	}
	if !strings.Contains(got, "head") || !strings.Contains(got, "tail") {
		t.Errorf("lost surrounding output:\n%q", got)
	}
}

func TestTruncateLeavesTextUnderBudget(t *testing.T) {
	text := "short enough\n"
	got, stats := Truncate(text, 1024, "")
	if got != text {
		t.Errorf("Truncate = %q, want it untouched", got)
	}
	if stats.TruncatedBytes != 0 {
		t.Errorf("TruncatedBytes = %d, want 0", stats.TruncatedBytes)
	}
}

func TestTruncateKeepsHeadAndTail(t *testing.T) {
	head := "FIRST LINE OF OUTPUT\n"
	tail := "\nFATAL: the thing that actually went wrong\n"
	text := head + strings.Repeat("filler line that nobody needs to read\n", 5000) + tail

	got, stats := Truncate(text, 2048, "")

	if !strings.HasPrefix(got, head) {
		t.Errorf("head lost:\n%q", got[:min(len(got), 200)])
	}
	if !strings.HasSuffix(got, tail) {
		t.Errorf("tail lost:\n%q", got[max(0, len(got)-200):])
	}
	if !strings.Contains(got, "output truncated") {
		t.Error("truncation happened without saying so")
	}
	if stats.TruncatedBytes == 0 {
		t.Error("TruncatedBytes = 0 after a truncation")
	}
	if stats.FinalBytes != len(got) {
		t.Errorf("FinalBytes = %d, want %d", stats.FinalBytes, len(got))
	}
}

func TestTruncateIncludesNote(t *testing.T) {
	text := strings.Repeat("x", 10000)
	got, _ := Truncate(text, 512, "full output: /workspace/.harness/tool-output/abc.txt")
	if !strings.Contains(got, "/workspace/.harness/tool-output/abc.txt") {
		t.Errorf("note missing from marker:\n%q", got)
	}
}

// A cut landing inside a multi-byte rune would put invalid UTF-8 into the
// request body, which the JSON encoder turns into replacement characters.
func TestTruncateNeverSplitsARune(t *testing.T) {
	text := strings.Repeat("héllo wörld ✓ 日本語\n", 2000)
	for _, budget := range []int{64, 100, 257, 999, 4096} {
		got, _ := Truncate(text, budget, "")
		if !utf8.ValidString(got) {
			t.Errorf("budget %d produced invalid UTF-8", budget)
		}
	}
}

func TestTruncatePrefersLineBoundaries(t *testing.T) {
	text := strings.Repeat("a line of predictable length here\n", 500)
	got, _ := Truncate(text, 1024, "")
	head, _, _ := strings.Cut(got, "…[output truncated")
	if !strings.HasSuffix(head, "\n\n") {
		t.Errorf("head did not end on a line boundary: %q", head[max(0, len(head)-80):])
	}
}

func TestReduceCombinesBothStages(t *testing.T) {
	text := "\x1b[32mstart\x1b[0m\n" + strings.Repeat("noisy repeated line\n", 4000)
	got, stats := Reduce(text, 512, "")

	if stats.OriginalBytes != len(text) {
		t.Errorf("OriginalBytes = %d, want %d", stats.OriginalBytes, len(text))
	}
	if stats.FinalBytes != len(got) {
		t.Errorf("FinalBytes = %d, want %d", stats.FinalBytes, len(got))
	}
	if stats.ANSIBytes == 0 || stats.RepeatBytes == 0 {
		t.Errorf("normalize stages not reported: %+v", stats)
	}
	// The repeat collapse alone gets this under budget, so nothing is lost.
	if stats.TruncatedBytes != 0 {
		t.Errorf("TruncatedBytes = %d, want the repeat collapse to have sufficed", stats.TruncatedBytes)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{3 << 20, "3.0 MiB"},
	} {
		if got := HumanBytes(tc.n); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
