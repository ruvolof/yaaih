package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/muesli/cancelreader"
	"golang.org/x/term"

	"github.com/ruvolof/yaaih/internal/executor"
	"github.com/ruvolof/yaaih/internal/logging"
)

// terminalToggleKey is ctrl+], the byte that both opens the operator's shell
// from the chat and, read from the keyboard while in it, returns to the chat.
const terminalToggleKey = 0x1d

// terminalBacklogBytes bounds the shell output kept while the operator is in
// the chat, replayed when they return to the shell.
const terminalBacklogBytes = 256 << 10

// approvalNotice is shown in the shell when the agent stops for an approval.
const approvalNotice = "\r\n\x1b[1;33m[yaaih] the agent is waiting for an approval — ctrl+] to return to the chat\x1b[0m\r\n"

// operatorTerminal keeps the operator's shell in the sandbox alive between
// visits, so whatever they were doing in it is still there when they return.
// While nobody is looking, the shell's output goes into a backlog instead.
type operatorTerminal struct {
	ctx    context.Context
	shells executor.OperatorShells
	relay  *eventRelay

	mu       sync.Mutex
	shell    *executor.OperatorShell
	exited   chan struct{} // closed when shell's output ends
	attached io.Writer     // the operator's screen while they are in the shell
	backlog  []byte
}

func newOperatorTerminal(ctx context.Context, shells executor.OperatorShells, relay *eventRelay) *operatorTerminal {
	return &operatorTerminal{ctx: ctx, shells: shells, relay: relay}
}

// visit is one stay in the shell, from ctrl+] to ctrl+]. It satisfies
// tea.ExecCommand: the program hands it the terminal for the duration of Run.
// approvalShown says an approval was already waiting in the chat.
func (t *operatorTerminal) visit(approvalShown bool) *terminalVisit {
	return &terminalVisit{terminal: t, approvalShown: approvalShown}
}

type terminalVisit struct {
	terminal      *operatorTerminal
	approvalShown bool
	stdin         io.Reader
	stdout        io.Writer
}

func (v *terminalVisit) SetStdin(r io.Reader)  { v.stdin = r }
func (v *terminalVisit) SetStdout(w io.Writer) { v.stdout = w }
func (v *terminalVisit) SetStderr(io.Writer)   {}

func (v *terminalVisit) Run() error {
	stdin, ok := v.stdin.(*os.File)
	if !ok || !term.IsTerminal(int(stdin.Fd())) {
		return errors.New("the operator shell needs an interactive terminal")
	}
	sizeFD := int(stdin.Fd())
	if stdout, ok := v.stdout.(*os.File); ok && term.IsTerminal(int(stdout.Fd())) {
		sizeFD = int(stdout.Fd())
	}
	cols, rows, err := term.GetSize(sizeFD)
	if err != nil {
		cols, rows = 80, 24
	}

	shell, exited, reopened, err := v.terminal.ensureShell(cols, rows)
	if err != nil {
		return err
	}
	previousState, err := term.MakeRaw(int(stdin.Fd()))
	if err != nil {
		return err
	}
	defer func() { _ = term.Restore(int(stdin.Fd()), previousState) }()

	v.terminal.attach(v.stdout)
	defer v.terminal.detach()
	if !reopened {
		// A shell that kept running drew its screen for whatever size the
		// terminal had then; a size change makes readline and full-screen
		// programs redraw. The kernel signals only an actual change, hence
		// the step through a different width.
		_ = shell.Resize(max(cols-1, 1), rows)
		_ = shell.Resize(cols, rows)
	}
	if v.terminal.relay != nil {
		// A signal left over from while the operator was in the chat is
		// about an approval they have already seen; what is waiting now is
		// checked directly.
		select {
		case <-v.terminal.relay.approvalQueued:
		default:
		}
	}
	if v.approvalShown || (v.terminal.relay != nil && v.terminal.relay.approvalWaiting()) {
		v.terminal.write([]byte(approvalNotice))
	}

	stop := make(chan struct{})
	defer close(stop)
	go watchResize(stop, sizeFD, func(cols, rows int) { _ = shell.Resize(cols, rows) })
	if v.terminal.relay != nil {
		go v.terminal.announceApprovals(stop)
	}

	keyboard, err := cancelreader.NewReader(stdin)
	if err != nil {
		return err
	}
	defer keyboard.Close()
	go func() {
		select {
		case <-exited:
			keyboard.Cancel()
		case <-stop:
		}
	}()
	return forwardKeys(keyboard, shell.Input)
}

// forwardKeys copies the keyboard to the shell until the toggle key, which is
// not passed on, or until the keyboard is cancelled because the shell exited.
func forwardKeys(keyboard io.Reader, shell io.Writer) error {
	buffer := make([]byte, 4096)
	for {
		n, err := keyboard.Read(buffer)
		if n > 0 {
			keys := buffer[:n]
			toggle := bytes.IndexByte(keys, terminalToggleKey)
			if toggle >= 0 {
				keys = keys[:toggle]
			}
			if len(keys) > 0 {
				if _, writeErr := shell.Write(keys); writeErr != nil {
					return nil
				}
			}
			if toggle >= 0 {
				return nil
			}
		}
		if err != nil {
			return nil
		}
	}
}

// ensureShell returns the open shell, or opens one at cols×rows. reopened
// reports a new shell, which has nothing on screen to redraw. A shell kept
// from an earlier visit has its files refreshed for yaaih-sync.
func (t *operatorTerminal) ensureShell(cols, rows int) (shell *executor.OperatorShell, exited chan struct{}, reopened bool, err error) {
	t.mu.Lock()
	shell, exited = t.shell, t.exited
	t.mu.Unlock()
	if shell != nil {
		if err := t.shells.PrepareOperatorShell(t.ctx); err != nil {
			log.Warn("could not refresh the operator shell's state", "err", err)
		}
		return shell, exited, false, nil
	}

	shell, err = t.shells.OpenOperatorShell(t.ctx, os.Getenv("TERM"), cols, rows)
	if err != nil {
		return nil, nil, false, err
	}
	exited = make(chan struct{})
	t.mu.Lock()
	t.shell, t.exited, t.backlog = shell, exited, nil
	t.mu.Unlock()
	go t.pump(shell, exited)
	return shell, exited, true, nil
}

// pump carries one shell's output to the operator's screen, or to the backlog
// while they are away, until the shell exits.
func (t *operatorTerminal) pump(shell *executor.OperatorShell, exited chan struct{}) {
	defer logging.Recover("tui.terminal")
	buffer := make([]byte, 32<<10)
	for {
		n, err := shell.Output.Read(buffer)
		if n > 0 {
			t.write(buffer[:n])
		}
		if err != nil {
			break
		}
	}
	shell.Close()
	t.mu.Lock()
	if t.shell == shell {
		t.shell, t.exited, t.backlog = nil, nil, nil
	}
	t.mu.Unlock()
	close(exited)
}

// write puts output on the operator's screen, or in the backlog while they
// are in the chat, keeping only its newest terminalBacklogBytes.
func (t *operatorTerminal) write(output []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.attached != nil {
		_, _ = t.attached.Write(output)
		return
	}
	t.backlog = append(t.backlog, output...)
	if excess := len(t.backlog) - terminalBacklogBytes; excess > 0 {
		t.backlog = append([]byte(nil), t.backlog[excess:]...)
	}
}

func (t *operatorTerminal) attach(screen io.Writer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.backlog) > 0 {
		_, _ = screen.Write(t.backlog)
		t.backlog = nil
	}
	t.attached = screen
}

func (t *operatorTerminal) detach() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attached = nil
}

// announceApprovals tells the operator, in the shell, each time the agent
// stops for an approval.
func (t *operatorTerminal) announceApprovals(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-t.relay.approvalQueued:
			t.write([]byte(approvalNotice))
		}
	}
}
