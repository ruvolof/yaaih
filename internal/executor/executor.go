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
//   - On command timeout or cancel we abandon the current bash session and
//     start a fresh one. Interrupting just the foreground child without a TTY
//     is fragile; a clean restart is more honest. CWD and any in-session env
//     vars are lost, which we surface to the model via the TimeoutError
//     message.
//   - Abandoning a session does not end what it was running: closing the
//     attached connection signals no process. Bash leads its own process
//     group, and every job it starts shares that group unless it was detached
//     with setsid, so the restart kills the group from a separate exec. A
//     server left running would otherwise keep its port, and every leftover
//     counts against the container's PID limit.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/client"

	"github.com/ruvolof/yaaih/internal/logging"
	"github.com/ruvolof/yaaih/internal/toolout"
)

var log = logging.For("executor")

// promptMarkPrefix starts every prompt sentinel, so a sentinel is recognisable
// when grepping logs or captured output.
const promptMarkPrefix = "__HARNESS_"

// newPromptMark returns the sentinel bash writes after every command so the
// reader knows where one command's output ends. It is random per session:
// command output is arbitrary text, and a fixed marker would end the read early
// for any command that happened to print it, such as grepping this source.
func newPromptMark() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return promptMarkPrefix + hex.EncodeToString(nonce) + "__", nil
}

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

// reportShellPID is the last line of the init script. Its output, the only
// output the init script produces, is bash's pid: the process group the restart
// kills.
const reportShellPID = `printf '%s\n' "$$"`

// bashInitScript prepares the bash session for headless use. We do NOT set
// PS1: bash only emits PS1 when running interactively, and we attach without
// a TTY so it would never be displayed. The executor emits its own sentinel
// line via writePromptSentinel after every command instead.
func bashInitScript() string {
	return "unset PROMPT_COMMAND HISTFILE\n" +
		"export PS2=''\n" +
		"stty -echo 2>/dev/null || true\n" +
		reportShellPID + "\n"
}

// parseShellPID reads the pid reportShellPID printed. Pids 0 and 1 are refused
// rather than trusted: as process groups they stand for the caller's own group
// and for every process there is.
func parseShellPID(output string) (int, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("executor: bash reported %q as its pid", strings.TrimSpace(output))
	}
	return pid, nil
}

// writePromptSentinel writes a single bash command that prints promptMark and
// the current working directory. Called after the init script and after every
// user command so readUntilPromptFrom always has a deterministic boundary to
// lock onto.
func writePromptSentinel(w io.Writer, promptMark string) error {
	_, err := fmt.Fprintf(w, "printf '\\n%s:%%s\\n' \"$PWD\"\n", promptMark)
	return err
}

// commandFileVar names the session variable that holds the command file's path
// for the duration of one command.
const commandFileVar = "__harness_command_file"

// syntaxRejectedNote follows bash's own parse error when a command is refused,
// so the model does not read the error as coming from a command that ran. It is
// echoed inside single quotes, so it cannot contain one.
const syntaxRejectedNote = "[not executed: bash rejected the syntax of this command, so none of it ran]"

// wrapCommand turns cmd into a single line that bash is guaranteed to parse.
// Written verbatim, an unbalanced quote or an open ( would leave bash waiting
// for more input and swallow the sentinel after it; as base64 the command
// cannot interfere with the line it travels on. The decoded file is checked
// with `bash -n` before anything runs, because sourcing stops at a syntax error
// only after executing every line in front of it. It is sourced rather than
// run as a child so cd and export persist in the session, and it reads
// /dev/null so a command waiting on stdin gets EOF instead of the sentinel.
//
// extglob is enabled for the check because the session may have turned it on,
// and a pattern like !(x) is a syntax error to a bash without it.
func wrapCommand(cmd string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(cmd))
	return fmt.Sprintf(
		`%[1]s=$(mktemp "${TMPDIR:-/tmp}/command.XXXXXX") && printf '%%s' '%[2]s' | base64 -d > "$%[1]s" && `+
			`if bash -O extglob -n "$%[1]s"; then . "$%[1]s" </dev/null; else echo '%[3]s'; fi; `+
			`rm -f "$%[1]s"; unset %[1]s`+"\n",
		commandFileVar, encoded, syntaxRejectedNote)
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

// bashOpener starts one bash process for the executor to drive, and stops what
// an abandoned one left running. Splitting it out keeps the sentinel protocol —
// the part with the invariants worth getting right — independent of how the
// process is attached.
type bashOpener interface {
	openBash(ctx context.Context) (*bashProcess, error)
	// killProcessGroup sends SIGKILL to every process in the group. A group
	// that no longer exists counts as stopped.
	killProcessGroup(ctx context.Context, pgid int) error
}

// session bundles the state of one underlying bash invocation. When a command
// times out we discard the current session and create a new one — that way no
// stale data, lingering reader goroutine, or half-killed child process can
// leak into subsequent commands.
type session struct {
	proc       *bashProcess
	stdinW     io.Writer
	stdoutR    *bufio.Reader
	lineCh     chan readResult // owned end-to-end by this session
	promptMark string
	pgid       int
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
	promptMark, err := newPromptMark()
	if err != nil {
		return nil, "", fmt.Errorf("executor: generate prompt marker: %w", err)
	}
	proc, err := e.opener.openBash(ctx)
	if err != nil {
		return nil, "", err
	}

	s := &session{
		proc:       proc,
		stdinW:     proc.stdin,
		stdoutR:    bufio.NewReader(proc.stdout),
		lineCh:     make(chan readResult, readerChanBuf),
		promptMark: promptMark,
	}
	go s.readerLoop()

	if _, err := io.WriteString(s.stdinW, bashInitScript()); err != nil {
		s.close()
		return nil, "", fmt.Errorf("executor: prime bash: %w", err)
	}
	if err := writePromptSentinel(s.stdinW, s.promptMark); err != nil {
		s.close()
		return nil, "", fmt.Errorf("executor: emit init prompt: %w", err)
	}
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	initOutput, cwd, _, err := readUntilPromptFrom(initCtx, s.lineCh, s.promptMark, maxCapturedBytes)
	if err != nil {
		s.close()
		return nil, "", fmt.Errorf("executor: read initial prompt: %w", err)
	}
	if s.pgid, err = parseShellPID(initOutput); err != nil {
		s.close()
		return nil, "", err
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
func readUntilPromptFrom(ctx context.Context, ch <-chan readResult, promptMark string, maxBytes int) (output, cwd string, discarded int, err error) {
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

// Execute runs cmd in the bash session and reads everything up to the next
// prompt. cmd may span several lines; one with a syntax error is refused
// without running any of it, and bash's error comes back as the output. On
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
	if _, err := io.WriteString(e.sess.stdinW, wrapCommand(cmd)); err != nil {
		log.Error("write cmd failed", "err", err)
		return "", fmt.Errorf("executor: write cmd: %w", err)
	}
	if err := writePromptSentinel(e.sess.stdinW, e.sess.promptMark); err != nil {
		log.Error("write prompt sentinel failed", "err", err)
		return "", fmt.Errorf("executor: emit prompt: %w", err)
	}

	cmdCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	out, cwd, discarded, err := readUntilPromptFrom(cmdCtx, e.sess.lineCh, e.sess.promptMark, maxCapturedBytes)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// Either way bash is still running the command and still owns stdin,
		// so the session cannot be reused — the next command would sit unread
		// behind this one. Abandon it, and kill what it was running.
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
		stopped, rErr := e.restartSession()
		if rErr != nil {
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
			Message:  timeoutMessage(e.timeout, stopped),
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

// timeoutMessage tells the model what a timeout cost it. Whether the command is
// still running decides what it can do next: retry it, or find it first.
func timeoutMessage(timeout time.Duration, stopped bool) string {
	fate := "It was killed along with everything it started, except jobs detached with setsid."
	if !stopped {
		fate = "It could not be stopped and may still be running, along with anything it started."
	}
	return fmt.Sprintf(
		"Command timed out after %s. %s The bash session was restarted, so CWD reset to %s and any session-local env vars were lost. "+
			"A command meant to keep running, such as a server, has to be started in the background: setsid cmd >log 2>&1 &. "+
			"Otherwise consider increasing command_timeout in agent.md.",
		timeout, fate, initialCWD)
}

// restartSession kills the current bash session's process group, closes the
// session, and opens a fresh one. stopped reports whether the kill took; a
// failed kill is logged and the restart goes ahead regardless. Caller must
// hold e.mu.
func (e *DockerExecutor) restartSession() (stopped bool, err error) {
	old := e.sess
	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()
	if killErr := e.opener.killProcessGroup(killCtx, old.pgid); killErr != nil {
		log.Warn("could not kill the abandoned session's processes", "pgid", old.pgid, "err", killErr)
	} else {
		stopped = true
	}
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
		return stopped, err
	}
	e.sess = sess
	e.cwd = cwd
	log.Info("bash session restarted", "cwd", cwd, "previous_processes_killed", stopped)
	return stopped, nil
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
