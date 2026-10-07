package executor

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"sync"
)

// fakeBash stands in for the bash running inside the sandbox container. It
// speaks the same protocol the real one does — read a line, act on it, answer
// the sentinel printf with the current directory — so everything above it runs
// unchanged and every branch that needs a container becomes reachable.
type fakeBash struct {
	mu       sync.Mutex
	replies  map[string]bashReply
	cwd      string
	opens    int
	commands []string

	// failOpenFrom is the 1-based session number from which openBash starts
	// failing, standing in for a container that has gone away. Zero never fails.
	failOpenFrom int
	openErr      error

	// killed is every process group the executor asked to have killed; killErr,
	// when set, is what each of those requests fails with.
	killed  []int
	killErr error
}

// bashReply is what the fake does with one command.
type bashReply struct {
	// output is written back verbatim, before the sentinel.
	output string
	// cwd, when set, is the directory reported from this command onwards, the
	// way a `cd` moves the session.
	cwd string
	// block models a command that has not returned: bash is still busy with it
	// and never reaches the sentinel, so this process answers nothing further.
	block bool
}

func newFakeBash(replies map[string]bashReply) *fakeBash {
	return &fakeBash{replies: replies, cwd: initialCWD}
}

func (f *fakeBash) openBash(ctx context.Context) (*bashProcess, error) {
	f.mu.Lock()
	f.opens++
	failing := f.failOpenFrom > 0 && f.opens >= f.failOpenFrom
	err := f.openErr
	pid := fakeShellPID(f.opens)
	f.mu.Unlock()
	if failing {
		return nil, err
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	go f.run(stdinR, stdoutW, pid)

	return &bashProcess{
		stdin:  stdinW,
		stdout: stdoutR,
		close: func() {
			stdinW.Close()
			stdinR.Close()
			stdoutW.Close()
			stdoutR.Close()
		},
	}, nil
}

func (f *fakeBash) killProcessGroup(ctx context.Context, pgid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, pgid)
	return f.killErr
}

// fakeShellPID is the pid the fake's nth session reports, distinct per session
// so a test can tell which one was killed.
func fakeShellPID(session int) int { return 1000 + session }

// sentinelRequest starts the line writePromptSentinel emits, as bash receives
// it; the session's marker follows, up to the colon.
const sentinelRequest = `printf '\n` + promptMarkPrefix

// promptMarkOf recovers the marker a sentinel request asks bash to print.
func promptMarkOf(line string) string {
	promptMark, _, _ := strings.Cut(strings.TrimPrefix(line, `printf '\n`), ":")
	return promptMark
}

// unwrapCommand recovers the command wrapCommand encoded into line, and reports
// false for any line that is not one.
func unwrapCommand(line string) (string, bool) {
	const encodedStart = `printf '%s' '`
	start := strings.Index(line, encodedStart)
	if start < 0 {
		return "", false
	}
	encoded := line[start+len(encodedStart):]
	end := strings.IndexByte(encoded, '\'')
	if end < 0 {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded[:end])
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func (f *fakeBash) run(stdin io.ReadCloser, stdout io.WriteCloser, pid int) {
	reader := bufio.NewReader(stdin)
	busy := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if busy {
			continue
		}
		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, sentinelRequest) {
			f.mu.Lock()
			cwd := f.cwd
			f.mu.Unlock()
			if _, err := io.WriteString(stdout, "\n"+promptMarkOf(line)+":"+cwd+"\n"); err != nil {
				return
			}
			continue
		}

		if line == reportShellPID {
			if _, err := io.WriteString(stdout, strconv.Itoa(pid)+"\n"); err != nil {
				return
			}
			continue
		}

		if command, wrapped := unwrapCommand(line); wrapped {
			line = command
		}

		f.mu.Lock()
		reply, scripted := f.replies[line]
		if scripted {
			f.commands = append(f.commands, line)
			if reply.cwd != "" {
				f.cwd = reply.cwd
			}
		}
		f.mu.Unlock()

		if reply.block {
			busy = true
			continue
		}
		if reply.output != "" {
			if _, err := io.WriteString(stdout, reply.output); err != nil {
				return
			}
		}
	}
}

// sessionsOpened counts handshakes, so a test can tell a restart from a reuse.
func (f *fakeBash) sessionsOpened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// killedGroups is every process group the executor asked to have killed.
func (f *fakeBash) killedGroups() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.killed...)
}

// executed is every scripted command that actually reached bash.
func (f *fakeBash) executed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}
