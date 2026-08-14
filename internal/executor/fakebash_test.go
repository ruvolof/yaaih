package executor

import (
	"bufio"
	"context"
	"io"
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
	f.mu.Unlock()
	if failing {
		return nil, err
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	go f.run(stdinR, stdoutW)

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

// sentinelRequest is the line writePromptSentinel emits, as bash receives it.
var sentinelRequest = `printf '\n` + promptMark

func (f *fakeBash) run(stdin io.ReadCloser, stdout io.WriteCloser) {
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
			if _, err := io.WriteString(stdout, "\n"+promptMark+":"+cwd+"\n"); err != nil {
				return
			}
			continue
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

// executed is every scripted command that actually reached bash.
func (f *fakeBash) executed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}
