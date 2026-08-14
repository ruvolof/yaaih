// Package executor provides a stateful bash session that persists CWD and
// environment variables across commands.
//
// The session is a single persistent `bash` started inside the runtime
// container via the Docker exec API. The harness writes commands to bash's
// stdin and reads output until a sentinel line is observed; the sentinel
// also carries the new CWD so we can keep the sidebar in sync.
//
// Concurrency model:
//
//   - Execute is serialised by e.mu, so callers can issue overlapping
//     Execute calls and they queue.
//   - Exactly one reader goroutine ever reads from the bash session's
//     stdout. It pushes each line onto a buffered channel; readUntilPrompt
//     consumes that channel. bufio.Reader is not safe for concurrent use,
//     so this is the only safe pattern.
//   - On command timeout we abandon the current bash session and start a
//     fresh one. Trying to SIGINT the foreground child without a TTY is
//     fragile (pkill semantics depend on parent/process-group state that
//     docker exec does not give us); a clean restart is more honest. CWD
//     and any in-session env vars are lost, which we surface to the model
//     via the TimeoutError message.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/client"

	"github.com/ruvolof/aibitat-harness-go/internal/logging"
	"github.com/ruvolof/aibitat-harness-go/internal/toolout"
)

var log = logging.For("executor")

// promptMark is a unique sentinel written by bash after every command so the
// reader knows where one command's output ends. The CWD is included in the
// sentinel so the executor can track it without an extra round-trip.
const promptMark = "__HARNESS_PROMPT__"

// readerChanBuf is the buffer size of the per-session line channel. Large
// enough that ordinary command output (a few hundred lines) never backpressures
// the reader; small enough that runaway output is bounded.
const readerChanBuf = 1024

// maxCapturedBytes bounds how much of one command's output the harness holds in
// memory. It is a ceiling against `cat` of a huge file, not a context budget —
// what the model actually receives is bounded far lower, further up the stack.
const maxCapturedBytes = 8 << 20

// ContainerWorkspace is where the host workspace is bind-mounted inside the
// sandbox. A path handed to the model has to be expressed against this root:
// the host path the harness writes to does not exist in the container the model
// runs its commands in.
const ContainerWorkspace = "/workspace"

// initialCWD is the working directory bash starts in. Mirrored in the exec
// config's WorkingDir below.
const initialCWD = ContainerWorkspace

// TimeoutError is returned when a command exceeds the executor's timeout. The
// TUI renders this distinctly so the operator can decide whether to raise the
// configured `command_timeout`.
type TimeoutError struct {
	Command  string
	Duration time.Duration
	Message  string
}

func (e *TimeoutError) Error() string { return e.Message }

// Executor is the contract used by the shell tool.
type Executor interface {
	Execute(ctx context.Context, cmd string) (string, error)
	CWD() string
	IsRunning() bool
	Close() error
}

// bashInitScript prepares the bash session for headless use. We do NOT set
// PS1: bash only emits PS1 when running interactively, and we attach without
// a TTY so it would never be displayed. The executor emits its own sentinel
// line via writePromptSentinel after every command instead.
func bashInitScript() string {
	return "unset PROMPT_COMMAND HISTFILE\n" +
		"export PS2=''\n" +
		"stty -echo 2>/dev/null || true\n"
}

// writePromptSentinel writes a single bash command that prints the prompt
// sentinel and the current working directory. Called after the init script
// and after every user command so readUntilPrompt always has a deterministic
// boundary to lock onto.
func writePromptSentinel(w io.Writer) error {
	_, err := fmt.Fprintf(w, "printf '\\n%s:%%s\\n' \"$PWD\"\n", promptMark)
	return err
}

// readResult is one item produced by the reader goroutine.
type readResult struct {
	line string
	err  error
}

// bashProcess is one attached bash: a writer for its stdin, a reader carrying
// its stdout and stderr already demultiplexed into a single stream, and a
// teardown that takes the process with it.
type bashProcess struct {
	stdin  io.Writer
	stdout io.Reader
	close  func()
}

// bashOpener starts one bash process for the executor to drive. Splitting it
// out keeps the sentinel protocol — the part with the invariants worth getting
// right — independent of how the process is attached.
type bashOpener interface {
	openBash(ctx context.Context) (*bashProcess, error)
}

// session bundles the state of one underlying bash invocation. When a command
// times out we discard the current session and create a new one — that way no
// stale data, lingering reader goroutine, or half-killed child process can
// leak into subsequent commands.
type session struct {
	proc    *bashProcess
	stdinW  io.Writer
	stdoutR *bufio.Reader
	lineCh  chan readResult // owned end-to-end by this session
}

// DockerExecutor runs the persistent bash inside the session container via
// the Docker exec API. The exec instance is hijacked so we get bidirectional
// stdio over a single connection.
type DockerExecutor struct {
	opener  bashOpener
	timeout time.Duration

	mu     sync.Mutex
	closed bool
	cwd    string
	sess   *session
}

// NewDockerExecutor starts a persistent bash inside the container and returns
// a ready-to-use Executor. The caller is responsible for keeping the container
// alive for the executor's lifetime.
func NewDockerExecutor(ctx context.Context, cli *client.Client, containerID string, timeout time.Duration) (*DockerExecutor, error) {
	if cli == nil {
		return nil, errors.New("executor: nil docker client")
	}
	if containerID == "" {
		return nil, errors.New("executor: empty container id")
	}

	e, err := newExecutor(ctx, dockerBash{cli: cli, containerID: containerID}, timeout)
	if err != nil {
		return nil, err
	}
	log.Info("bash session started",
		"container_id", containerID[:12],
		"cwd", e.cwd,
		"timeout", timeout.String(),
	)
	return e, nil
}

// newExecutor drives the first handshake and leaves the executor ready for
// commands.
func newExecutor(ctx context.Context, opener bashOpener, timeout time.Duration) (*DockerExecutor, error) {
	e := &DockerExecutor{
		opener:  opener,
		timeout: timeout,
		cwd:     initialCWD,
	}
	sess, cwd, err := e.openSession(ctx)
	if err != nil {
		return nil, err
	}
	e.sess = sess
	e.cwd = cwd
	return e, nil
}

// openSession starts a fresh bash, starts the reader goroutine, and primes
// bash so the first prompt sentinel arrives. Used both by the constructor and
// by the timeout-recovery restart path.
func (e *DockerExecutor) openSession(ctx context.Context) (*session, string, error) {
	proc, err := e.opener.openBash(ctx)
	if err != nil {
		return nil, "", err
	}

	s := &session{
		proc:    proc,
		stdinW:  proc.stdin,
		stdoutR: bufio.NewReader(proc.stdout),
		lineCh:  make(chan readResult, readerChanBuf),
	}
	go s.readerLoop()

	if _, err := io.WriteString(s.stdinW, bashInitScript()); err != nil {
		s.close()
		return nil, "", fmt.Errorf("executor: prime bash: %w", err)
	}
	if err := writePromptSentinel(s.stdinW); err != nil {
		s.close()
		return nil, "", fmt.Errorf("executor: emit init prompt: %w", err)
	}
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, cwd, _, err := readUntilPromptFrom(initCtx, s.lineCh, maxCapturedBytes)
	if err != nil {
		s.close()
		return nil, "", fmt.Errorf("executor: read initial prompt: %w", err)
	}
	return s, cwd, nil
}

// readerLoop is the single producer that reads from the session's stdout.
// One per session, never more than one alive at a time. Exits when the
// underlying pipe returns an error (typically EOF when hijack is closed).
func (s *session) readerLoop() {
	defer logging.Recover("executor.reader")
	for {
		line, err := s.stdoutR.ReadString('\n')
		if line != "" {
			s.lineCh <- readResult{line: line}
		}
		if err != nil {
			// Last gasp: try to deliver the error. If the channel buffer is
			// full and nobody is consuming (e.g. we're being torn down), drop
			// the marker and exit instead of blocking forever.
			select {
			case s.lineCh <- readResult{err: err}:
			default:
			}
			return
		}
	}
}

// close tears down a session: ending the attached process causes the
// underlying read in readerLoop to return EOF, which makes the reader exit.
func (s *session) close() {
	if s == nil {
		return
	}
	s.proc.close()
}

// drainPending discards any lines currently buffered on the session's
// channel. Called before sending a new command so stale output from a
// previous timed-out command can't be misread as the new command's result.
func (s *session) drainPending() {
	for {
		select {
		case <-s.lineCh:
		default:
			return
		}
	}
}

// readUntilPromptFrom consumes lines from ch until one contains promptMark.
// Cancellable via ctx; if ctx expires it returns whatever has been buffered
// so far plus ctx.Err().
//
// maxBytes caps what is retained, not what is consumed: the loop must reach the
// sentinel either way, or the abandoned tail of this command's output would be
// read as the next command's result. Whole lines are dropped rather than split,
// so the retained text is never cut mid-rune. Zero means no cap.
func readUntilPromptFrom(ctx context.Context, ch <-chan readResult, maxBytes int) (output, cwd string, discarded int, err error) {
	var buf bytes.Buffer
	keep := func(line string) {
		if maxBytes > 0 && buf.Len()+len(line) > maxBytes {
			discarded += len(line)
			return
		}
		buf.WriteString(line)
	}
	for {
		select {
		case <-ctx.Done():
			return buf.String(), "", discarded, ctx.Err()
		case rr := <-ch:
			if rr.err != nil {
				return buf.String(), "", discarded, rr.err
			}
			line := rr.line
			if idx := strings.Index(line, promptMark+":"); idx >= 0 {
				keep(line[:idx])
				rest := strings.TrimRight(line[idx+len(promptMark)+1:], "\r\n")
				return buf.String(), rest, discarded, nil
			}
			keep(line)
		}
	}
}

// Execute writes cmd to bash and reads everything up to the next prompt. On
// timeout the current bash session is abandoned and a fresh one is started;
// the caller receives a *TimeoutError.
func (e *DockerExecutor) Execute(ctx context.Context, cmd string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", errors.New("executor: closed")
	}

	// Discard any stale output left on the channel before sending — defensive
	// against a previous timed-out command that may have produced lines after
	// we abandoned its session.
	e.sess.drainPending()

	log.Debug("send", "cmd", truncate(cmd, 200), "cwd", e.cwd)
	start := time.Now()
	if _, err := io.WriteString(e.sess.stdinW, cmd+"\n"); err != nil {
		log.Error("write cmd failed", "err", err)
		return "", fmt.Errorf("executor: write cmd: %w", err)
	}
	if err := writePromptSentinel(e.sess.stdinW); err != nil {
		log.Error("write prompt sentinel failed", "err", err)
		return "", fmt.Errorf("executor: emit prompt: %w", err)
	}

	cmdCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	out, cwd, discarded, err := readUntilPromptFrom(cmdCtx, e.sess.lineCh, maxCapturedBytes)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// Either way bash is still running the command and still owns stdin,
		// so the session cannot be reused — the next command would sit unread
		// behind this one. Abandon it; the docker exec takes the child with it.
		cancelled := ctx.Err() != nil
		reason := "timed out"
		if cancelled {
			reason = "cancelled"
		}
		log.Warn("command abandoned, restarting bash session",
			"cmd", truncate(cmd, 200),
			"reason", reason,
			"timeout", e.timeout.String(),
			"out_bytes", len(out),
		)
		if rErr := e.restartSession(); rErr != nil {
			// Restart failed — the executor is now unusable. Mark closed so
			// subsequent calls fail fast.
			log.Error("restart after abandoned command failed; marking executor closed", "err", rErr)
			e.closed = true
		}
		if cancelled {
			return out, fmt.Errorf("executor: command cancelled: %w", ctx.Err())
		}
		return out, &TimeoutError{
			Command:  cmd,
			Duration: e.timeout,
			Message: fmt.Sprintf(
				"Command timed out after %s. The bash session was restarted, so CWD reset to %s and any session-local env vars were lost. Consider increasing command_timeout in agent.md.",
				e.timeout, initialCWD),
		}
	}
	if err != nil {
		log.Error("read prompt failed", "err", err, "out_bytes", len(out))
		return out, err
	}
	if cwd != "" && cwd != e.cwd {
		log.Debug("cwd changed", "from", e.cwd, "to", cwd)
		e.cwd = cwd
	}
	if discarded > 0 {
		log.Warn("command output exceeded the capture cap",
			"cmd", truncate(cmd, 100),
			"discarded_bytes", discarded,
			"cap_bytes", maxCapturedBytes,
		)
		out += fmt.Sprintf("\n[%s of output was dropped as it arrived: this command exceeded the executor's %s capture cap. Re-run it filtered — grep, head, tail — to see the part you need.]\n",
			toolout.HumanBytes(discarded), toolout.HumanBytes(maxCapturedBytes))
	}
	log.Info("result",
		"cmd", truncate(cmd, 100),
		"out_bytes", len(out),
		"discarded_bytes", discarded,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"cwd", e.cwd,
	)
	return out, nil
}

// restartSession closes the current bash session (any in-flight child
// processes go with it when the docker exec terminates) and opens a fresh
// one. Caller must hold e.mu.
func (e *DockerExecutor) restartSession() error {
	old := e.sess
	old.close()

	// Best-effort: wait briefly for the old reader goroutine to drain its
	// final EOF onto its own channel. This is purely hygiene — the channel
	// is owned by the old session and goes away with it.
	drainCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	for {
		select {
		case <-drainCtx.Done():
			goto open
		case rr := <-old.lineCh:
			if rr.err != nil {
				goto open
			}
		}
	}

open:
	openCtx, openCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer openCancel()
	sess, cwd, err := e.openSession(openCtx)
	if err != nil {
		return err
	}
	e.sess = sess
	e.cwd = cwd
	log.Info("bash session restarted", "cwd", cwd)
	return nil
}

// truncate is a small helper for log fields so we don't dump multi-KB
// commands or outputs into the log file by accident.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (e *DockerExecutor) CWD() string { e.mu.Lock(); defer e.mu.Unlock(); return e.cwd }

func (e *DockerExecutor) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed
}

func (e *DockerExecutor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.sess != nil {
		e.sess.close()
	}
	return nil
}
