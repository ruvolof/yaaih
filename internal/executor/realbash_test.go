package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// localBash attaches a bash running on the test host, for the behaviour that
// only a real parser can show: how multi-line input, syntax errors and stdin
// are handled.
type localBash struct {
	dir string
}

func (l localBash) openBash(ctx context.Context) (*bashProcess, error) {
	cmd := exec.Command("bash", "--norc", "--noprofile")
	cmd.Dir = l.dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outputR, outputW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = outputW
	cmd.Stderr = outputW
	if err := cmd.Start(); err != nil {
		outputR.Close()
		outputW.Close()
		return nil, err
	}
	outputW.Close()

	return &bashProcess{
		stdin:  stdin,
		stdout: outputR,
		close: func() {
			stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			outputR.Close()
		},
	}, nil
}

func newLocalExecutor(t *testing.T, timeout time.Duration) *DockerExecutor {
	t.Helper()
	for _, tool := range []string{"bash", "base64", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH", tool)
		}
	}
	exec, err := newExecutor(context.Background(), localBash{dir: t.TempDir()}, timeout)
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	t.Cleanup(func() { exec.Close() })
	return exec
}

func TestHeredocRunsInsteadOfWaitingForMoreInput(t *testing.T) {
	exec := newLocalExecutor(t, 5*time.Second)

	out := execute(t, exec, "cat <<'EOF'\nhello\nEOF")

	if strings.TrimSpace(out) != "hello" {
		t.Errorf("output = %q, want hello", out)
	}
}

// Sourcing a file with a syntax error runs every line in front of the error
// before failing, so unbalanced input has to be refused before any of it runs.
func TestUnbalancedInputIsRefusedWithoutRunningAnyOfIt(t *testing.T) {
	for name, command := range map[string]string{
		"open quote":    "touch ran\necho \"unterminated",
		"open subshell": "touch ran\n( echo inside",
		"open if":       "touch ran\nif true; then echo inside",
	} {
		t.Run(name, func(t *testing.T) {
			exec := newLocalExecutor(t, 5*time.Second)
			execute(t, exec, "mkdir sub && cd sub")
			cwdBefore := exec.CWD()

			start := time.Now()
			out := execute(t, exec, command)

			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("took %s, want the refusal to be immediate", elapsed)
			}
			if !strings.Contains(out, syntaxRejectedNote) {
				t.Errorf("output = %q, want it to say the command was not executed", out)
			}
			if _, err := os.Stat(cwdBefore + "/ran"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the line before the syntax error ran (stat err = %v)", err)
			}
			if exec.CWD() != cwdBefore {
				t.Errorf("CWD = %q, want the session kept at %q", exec.CWD(), cwdBefore)
			}
		})
	}
}

func TestMultiLineCommandKeepsSessionState(t *testing.T) {
	exec := newLocalExecutor(t, 5*time.Second)

	execute(t, exec, "mkdir -p nested/dir\ncd nested/dir\nexport GREETING=hi")

	if !strings.HasSuffix(exec.CWD(), "/nested/dir") {
		t.Errorf("CWD = %q, want the cd inside the command to persist", exec.CWD())
	}
	if out := execute(t, exec, `echo "$GREETING"`); strings.TrimSpace(out) != "hi" {
		t.Errorf("output = %q, want the export inside the command to persist", out)
	}
}

func TestCommandReadingStdinGetsEndOfInput(t *testing.T) {
	exec := newLocalExecutor(t, 5*time.Second)

	out := execute(t, exec, "cat; read -r answer; echo \"read status $?\"")

	if strings.TrimSpace(out) != "read status 1" {
		t.Errorf("output = %q, want cat and read to see an empty stdin", out)
	}
}

// Parsed without extglob, a pattern like !(x) is a syntax error, so a session
// that turned it on must not have its commands refused by the check.
func TestExtglobPatternsPassTheSyntaxCheck(t *testing.T) {
	exec := newLocalExecutor(t, 5*time.Second)
	execute(t, exec, "shopt -s extglob\ntouch keep.txt drop.log")

	out := execute(t, exec, "ls !(*.log)")

	if strings.TrimSpace(out) != "keep.txt" {
		t.Errorf("output = %q, want the extglob pattern to have run", out)
	}
}

// The wrapper keeps the command file's path in a session variable, so it must
// not be one a model is likely to use for itself.
func TestSessionVariablesSurviveTheWrapper(t *testing.T) {
	exec := newLocalExecutor(t, 5*time.Second)
	execute(t, exec, "f=mine\nfile=also-mine")

	out := execute(t, exec, `echo "$f $file"`)

	if strings.TrimSpace(out) != "mine also-mine" {
		t.Errorf("output = %q, want the session's own variables intact", out)
	}
}

// A command printing something shaped like the sentinel — grepping this
// package's source, say — must get all of its output back and leave the
// tracked CWD alone.
func TestOutputShapedLikeASentinelDoesNotEndTheCommand(t *testing.T) {
	exec := newLocalExecutor(t, 5*time.Second)
	cwdBefore := exec.CWD()

	out := execute(t, exec, "echo before\necho '__HARNESS_PROMPT__:/elsewhere'\necho after")

	if want := "before\n__HARNESS_PROMPT__:/elsewhere\nafter"; strings.TrimSpace(out) != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if exec.CWD() != cwdBefore {
		t.Errorf("CWD = %q, want it kept at %q", exec.CWD(), cwdBefore)
	}
	if out := execute(t, exec, "echo next"); strings.TrimSpace(out) != "next" {
		t.Errorf("next command's output = %q, want only its own", out)
	}
}
