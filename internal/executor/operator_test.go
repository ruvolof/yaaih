package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH")
	}
}

// The operator's shell gets the session's variables, functions, and
// directory, but not its set options — errexit would close an interactive
// shell at the first failing command — and keeps its own TERM.
func TestOperatorStateRestoresTheSessionForAnInteractiveShell(t *testing.T) {
	requireBash(t)
	cwd := t.TempDir()
	state := &shellState{
		cwd:          cwd,
		declarations: "declare -x GREETING=\"hi\"\ndeclare -x TERM=\"dumb\"\ngreet () \n{ \n    echo hello\n}\n",
		shopts:       "shopt -s extglob\n",
		setopts:      "errexit:nounset",
	}
	script := filepath.Join(t.TempDir(), "state.sh")
	if err := os.WriteFile(script, []byte(operatorStateScript(state)), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("bash", "--norc", "--noprofile", "-c",
		`__yaaih_term=xterm-256color; . "$1"; echo "$GREETING $TERM $PWD"; greet; shopt -q extglob && echo extglob; [[ $- == *e* ]] || echo no-errexit`,
		"bash", script).CombinedOutput()

	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	want := "hi xterm-256color " + cwd + "\nhello\nextglob\nno-errexit\n"
	if string(out) != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestOperatorHistoryKeepsMultiLineCommandsWhole(t *testing.T) {
	requireBash(t)
	history := filepath.Join(t.TempDir(), "history")
	content := operatorHistory([]HistoryEntry{
		{Command: "echo one", Started: time.Unix(1700000000, 0)},
		{Command: "for i in 1 2; do\n  echo $i\ndone\n", Started: time.Unix(1700000001, 0)},
	})
	if err := os.WriteFile(history, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Interactive, because that is how the operator's shell reads it.
	out, err := exec.Command("bash", "--norc", "--noprofile", "-ic",
		`HISTFILE="$1"; HISTTIMEFORMAT='@'; shopt -s cmdhist lithist; history -c; history -r; history`,
		"bash", history).Output()

	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	if entries := strings.Count(string(out), "@"); entries != 2 {
		t.Errorf("history = %q, want two entries", out)
	}
	if !strings.Contains(string(out), "@for i in 1 2; do\n  echo $i\ndone\n") {
		t.Errorf("history = %q, want the loop as one entry", out)
	}
}
