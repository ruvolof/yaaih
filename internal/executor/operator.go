package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// operatorDir holds the files an operator shell starts from.
const operatorDir = "/tmp/yaaih/operator"

// OperatorShells is implemented by an executor that can open an interactive
// shell for the operator in the sandbox, started from the session's state.
type OperatorShells interface {
	// PrepareOperatorShell writes the session's current state and the
	// model's command history where an operator shell reads them: on start,
	// and again when the operator runs yaaih-sync.
	PrepareOperatorShell(ctx context.Context) error
	// OpenOperatorShell prepares and starts an interactive shell on a
	// terminal of cols×rows. term is the operator's TERM.
	OpenOperatorShell(ctx context.Context, term string, cols, rows int) (*OperatorShell, error)
}

// OperatorShell is an interactive bash on a pseudo-terminal in the sandbox.
type OperatorShell struct {
	// Output carries what the terminal displays; it ends when the shell exits.
	Output io.Reader
	// Input is what the operator types.
	Input io.Writer

	resize    func(cols, rows int) error
	closeOnce sync.Once
	close     func()
}

// Resize tells the shell's terminal its new size.
func (s *OperatorShell) Resize(cols, rows int) error { return s.resize(cols, rows) }

// Close ends the shell's connection; the shell gets a hangup.
func (s *OperatorShell) Close() { s.closeOnce.Do(s.close) }

// ttyHost is a shellHost that can also start a process on a pseudo-terminal.
type ttyHost interface {
	startTTY(ctx context.Context, cmd, env []string, cols, rows int) (*OperatorShell, error)
}

func (e *JobExecutor) PrepareOperatorShell(ctx context.Context) error {
	e.mu.Lock()
	state := e.state
	history := append([]HistoryEntry(nil), e.history...)
	e.mu.Unlock()

	delimiter, err := newDelimiter()
	if err != nil {
		return err
	}
	var script strings.Builder
	fmt.Fprintf(&script, "command -p mkdir -p %s || exit 1\n", operatorDir)
	for _, file := range []struct{ name, content string }{
		{"state.sh", operatorStateScript(state)},
		{"history", operatorHistory(history)},
		{"rc", operatorRC},
		{"sync.sh", operatorSync},
	} {
		// >| because the session may have turned on noclobber.
		fmt.Fprintf(&script, "command -p cat >| %s/%s <<'%s' || exit 1\n%s\n%s\n", operatorDir, file.name, delimiter, file.content, delimiter)
	}
	result, err := e.Run(ctx, script.String())
	if err != nil {
		return fmt.Errorf("executor: write the operator shell's files: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("executor: write the operator shell's files: %s", truncate(strings.TrimSpace(result.Output), 200))
	}
	return nil
}

func (e *JobExecutor) OpenOperatorShell(ctx context.Context, term string, cols, rows int) (*OperatorShell, error) {
	host, ok := e.host.(ttyHost)
	if !ok {
		return nil, errors.New("executor: this sandbox cannot open a terminal")
	}
	if err := e.PrepareOperatorShell(ctx); err != nil {
		return nil, err
	}
	if term == "" {
		term = "xterm-256color"
	}
	return host.startTTY(ctx,
		[]string{"bash", "--rcfile", operatorDir + "/rc", "-i"},
		[]string{"TERM=" + term, "YAAIH_OPERATOR_TERM=" + term},
		cols, rows)
}

// operatorStateScript recreates the session's state in an interactive shell:
// everything a job restores except the `set -o` options, which in an
// interactive shell would do harm — errexit would close it at the first
// failing command. The terminal settings an interactive shell needs are put
// back on top: the session's own TERM is the dumb one jobs run with.
func operatorStateScript(state *shellState) string {
	var script strings.Builder
	if state != nil {
		script.WriteString("{\nunset $(compgen -e)\n")
		script.WriteString(state.declarations)
		script.WriteString("\n")
		script.WriteString(state.shopts)
		script.WriteString("\n} 2>/dev/null\n")
		fmt.Fprintf(&script, "builtin cd -- %s 2>/dev/null\n", shellQuote(state.cwd))
	}
	script.WriteString("shopt -s expand_aliases checkwinsize promptvars progcomp histappend cmdhist lithist 2>/dev/null\n")
	script.WriteString("export TERM=\"$__yaaih_term\"\n")
	return script.String()
}

// operatorHistory writes the model's commands as a bash history file. The
// timestamp line in front of each is what lets bash, with lithist, read a
// multi-line command back as one entry.
func operatorHistory(history []HistoryEntry) string {
	var file strings.Builder
	for _, entry := range history {
		fmt.Fprintf(&file, "#%d\n%s\n", entry.Started.Unix(), strings.TrimRight(entry.Command, "\n"))
	}
	return file.String()
}

// operatorSync reloads the session's state and history into a running
// operator shell.
const operatorSync = `. ` + operatorDir + `/state.sh
history -c
history -r
builtin printf '%s\n' "[yaaih] reloaded the agent's current directory, variables, functions, and command history"
`

// operatorRC starts the operator's shell. YAAIH_OPERATOR_TERM is kept
// unexported in __yaaih_term so that reloading the session's environment,
// which replaces every exported variable, cannot take it away. yaaih-sync is
// an alias rather than a function: sourced from a function, the state's
// declare lines would make every variable local to it.
const operatorRC = `__yaaih_term=${YAAIH_OPERATOR_TERM:-xterm-256color}
unset YAAIH_OPERATOR_TERM
. ` + operatorDir + `/state.sh
HISTFILE=` + operatorDir + `/history
HISTSIZE=10000
HISTFILESIZE=10000
HISTTIMEFORMAT='%F %T  '
alias yaaih-sync='. ` + operatorDir + `/sync.sh'
PS1='\[\e[1;33m\]yaaih\[\e[0m\]:\w\$ '
builtin printf '\e[1;33m%s\e[0m\n%s\n%s\n' \
	"yaaih operator shell — ctrl+] returns to the chat" \
	"Loaded with the agent's current directory, variables, and functions; its commands are in the history (up arrow, ctrl+r). yaaih-sync reloads both." \
	"Nothing done here changes the agent's session. Job logs: ` + ContainerWorkspace + `/` + JobLogDir + `/"
`
