package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, dir, name string, count int) string {
	t.Helper()
	var content strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestReadFileWindowReturnsRequestedRange(t *testing.T) {
	path := writeLines(t, t.TempDir(), "numbers.txt", 1000)

	window, total, err := readFileWindow(path, 101, 5)
	if err != nil {
		t.Fatal(err)
	}

	if total != 1000 {
		t.Errorf("total = %d, want 1000", total)
	}
	if window != "line 101\nline 102\nline 103\nline 104\nline 105\n" {
		t.Errorf("window = %q", window)
	}
}

func TestReadFileWindowClampsToEndOfFile(t *testing.T) {
	path := writeLines(t, t.TempDir(), "short.txt", 3)

	window, total, err := readFileWindow(path, 2, 500)
	if err != nil {
		t.Fatal(err)
	}

	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if window != "line 2\nline 3\n" {
		t.Errorf("window = %q, want the last two lines", window)
	}
}

func TestReadFileWindowPastEndIsNotAnError(t *testing.T) {
	path := writeLines(t, t.TempDir(), "short.txt", 3)

	window, total, err := readFileWindow(path, 99, 10)
	if err != nil {
		t.Fatalf("reading past the end should describe the file, not fail: %v", err)
	}
	if window != "" || total != 3 {
		t.Errorf("window = %q, total = %d, want empty window and total 3", window, total)
	}
}

func TestReadFileWindowCountsFinalLineWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonewline.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}

	window, total, err := readFileWindow(path, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if window != "alpha\nbeta" {
		t.Errorf("window = %q", window)
	}
}

// bufio.Scanner would fail here with "token too long"; a file the harness can
// write is a file it must be able to read back.
func TestReadFileWindowHandlesVeryLongLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oneline.json")
	giant := strings.Repeat("x", 20<<20)
	if err := os.WriteFile(path, []byte(giant+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	window, total, err := readFileWindow(path, 1, 10)
	if err != nil {
		t.Fatalf("readFileWindow: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(window) != len(giant)+1 {
		t.Errorf("len(window) = %d, want %d", len(window), len(giant)+1)
	}
}

func TestDescribeWindowTellsTheModelHowToContinue(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		offset, limit, total int
		want                 string
	}{
		{"more to come", 1, 500, 42318, "[app.log: lines 1-500 of 42318 — call again with offset 501 for the next page]\n"},
		{"final page", 42001, 500, 42318, "[app.log: lines 42001-42318 of 42318]\n"},
		{"whole file", 1, 500, 12, "[app.log: lines 1-12 of 12]\n"},
		{"past the end", 900, 500, 12, "[app.log: offset 900 is past the end; the file has 12 lines]\n"},
		{"empty", 1, 500, 0, "[app.log: empty file]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeWindow("app.log", tc.offset, tc.limit, tc.total); got != tc.want {
				t.Errorf("describeWindow = %q, want %q", got, tc.want)
			}
		})
	}
}

// End to end through the tool: a model reading a large file gets a page and is
// told how to get the rest, rather than getting the file and blowing the budget.
func TestFileReadToolPaginates(t *testing.T) {
	provider := &stubProvider{}
	agent := newTestAgent(t, []string{"filesystem"}, provider, nil)
	writeLines(t, agent.cfg.Runtime.Workspace, "big.log", 10000)

	tool, ok := agent.Registry().Enabled("file_read", []string{"filesystem"})
	if !ok {
		t.Fatal("file_read not registered")
	}

	first, err := tool.Fn(t.Context(), `{"path":"big.log"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "lines 1-50 of 10000") {
		t.Errorf("first page header wrong: %q", firstLine(first))
	}
	if !strings.Contains(first, "offset 51") {
		t.Errorf("first page does not say how to continue: %q", firstLine(first))
	}
	if strings.Contains(first, "line 51\n") {
		t.Error("first page ran past its limit")
	}

	second, err := tool.Fn(t.Context(), `{"path":"big.log","offset":51,"limit":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "lines 51-60 of 10000") {
		t.Errorf("second page header wrong: %q", firstLine(second))
	}
	if !strings.Contains(second, "line 51\n") || strings.Contains(second, "line 61\n") {
		t.Errorf("second page window wrong: %q", second)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
