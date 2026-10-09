package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadRecordsReturnsTheReportedState(t *testing.T) {
	stream := "pid 4242\nexit 3\ncwd 9\n/a\nb dir/state 24\nx=$'one\\ntwo'\nalias a=b\nshopts 18\nshopt -s extglob\n\nsetopts 15\nerrexit:nounsetend\n"
	var pgid int

	report, err := readRecords(strings.NewReader(stream), func(pid int) { pgid = pid })

	if err != nil {
		t.Fatalf("readRecords: %v", err)
	}
	if pgid != 4242 {
		t.Errorf("pid = %d, want 4242", pgid)
	}
	if !report.ended || report.exitCode != 3 {
		t.Errorf("report = %+v, want ended with exit 3", report)
	}
	want := shellState{
		cwd:          "/a\nb dir/",
		declarations: "x=$'one\\ntwo'\nalias a=b\n",
		shopts:       "shopt -s extglob\n\n",
		setopts:      "errexit:nounset",
	}
	if *report.state != want {
		t.Errorf("state = %+v, want %+v", *report.state, want)
	}
}

// A shell killed before its end record leaves no state to commit, and the
// stream simply stops.
func TestReadRecordsWithoutAnEndReportsNoState(t *testing.T) {
	report, err := readRecords(strings.NewReader("pid 4242\nexit 0\ncwd 4\n/tmp"), func(int) {})

	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if report.ended || report.state != nil {
		t.Errorf("report = %+v, want no end and no state", report)
	}
}

// The pid is the process group a kill is aimed at: anything but a real pid
// above 1 would point the kill at the wrong processes.
func TestReadRecordsRefusesWhatIsNotAProcessGroup(t *testing.T) {
	for _, pid := range []string{"", "0", "1", "-7", "x"} {
		called := false
		if _, err := readRecords(strings.NewReader("pid "+pid+"\n"), func(int) { called = true }); err == nil || called {
			t.Errorf("pid %q: err %v, called %v; want it refused", pid, err, called)
		}
	}
}

func TestSetOptionsPrefixRestoresEveryCarriedOption(t *testing.T) {
	state := shellState{setopts: "braceexpand:errexit:hashall:xtrace"}

	prefix := state.setOptionsPrefix()

	for _, want := range []string{"set -o errexit; ", "set +o nounset; ", "set -o xtrace; "} {
		if !strings.Contains(prefix, want) {
			t.Errorf("prefix = %q, want it to contain %q", prefix, want)
		}
	}
	if strings.Contains(prefix, "\n") {
		t.Errorf("prefix = %q, want one line, so the command's line numbers stay its own", prefix)
	}
	if !strings.HasSuffix(prefix, "set -o xtrace; ") {
		t.Errorf("prefix = %q, want xtrace restored last", prefix)
	}
}

func TestDescribeProcessGroupLeavesOutTheJobShell(t *testing.T) {
	listing := "  100   100 Ss    0.0 bash\n  101   100 S     0.3 npm\n  102   100 R    97.4 node\n  200   200 S     5.0 other\n"

	if got, want := describeProcessGroup(listing, 100), "2 processes: npm (sleeping), node (running, 97% cpu)"; got != want {
		t.Errorf("describeProcessGroup = %q, want %q", got, want)
	}
	if got := describeProcessGroup(listing, 300); got != "no processes besides the job shell" {
		t.Errorf("describeProcessGroup of an empty group = %q", got)
	}
}

func TestShellQuoteSurvivesSingleQuotes(t *testing.T) {
	if got, want := shellQuote("it's"), `'it'\''s'`; got != want {
		t.Errorf("shellQuote = %s, want %s", got, want)
	}
}

func TestDelimitersAreUnique(t *testing.T) {
	first, err := newDelimiter()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := newDelimiter()
	if first == second {
		t.Errorf("two delimiters are both %q", first)
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

// failingHost stands in for a container that cannot run a job at all.
type failingHost struct {
	startErr error
	// meta is what the job shell reports when start succeeds.
	meta string
}

func (f failingHost) startJob(ctx context.Context, script string) (*jobProcess, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &jobProcess{
		output: strings.NewReader("motd\n"),
		meta:   strings.NewReader(f.meta),
		close:  func() {},
	}, nil
}

func (f failingHost) runHelper(ctx context.Context, script string) (string, error) {
	return "", errors.New("no helper")
}

func TestExecutorFailsWhenTheContainerCannotRunAShell(t *testing.T) {
	_, err := newExecutor(context.Background(), failingHost{startErr: errors.New("no such container")}, t.TempDir())

	if err == nil || !strings.Contains(err.Error(), "no such container") {
		t.Errorf("err = %v, want the start failure", err)
	}
}

func TestExecutorFailsWhenTheBaselineReportsNoState(t *testing.T) {
	_, err := newExecutor(context.Background(), failingHost{meta: "pid 4242\n"}, t.TempDir())

	if err == nil || !strings.Contains(err.Error(), "motd") {
		t.Errorf("err = %v, want it to say the baseline reported nothing, with its output", err)
	}
}

func TestNewDockerExecutorRejectsAnIncompleteTarget(t *testing.T) {
	if _, err := NewDockerExecutor(context.Background(), nil, "abc", t.TempDir()); err == nil {
		t.Error("nil client accepted")
	}
}
