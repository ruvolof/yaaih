package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

func newTestExecutor(t *testing.T, bash *fakeBash, timeout time.Duration) *DockerExecutor {
	t.Helper()
	exec, err := newExecutor(context.Background(), bash, timeout)
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	t.Cleanup(func() { exec.Close() })
	return exec
}

// withSentinelLead is a command's output as the executor captures it. The
// sentinel printf leads with a newline so it always lands on a line of its own,
// and that newline is the last thing the previous command's output picks up.
func withSentinelLead(output string) string { return output + "\n" }

func execute(t *testing.T, exec *DockerExecutor, cmd string) string {
	t.Helper()
	out, err := exec.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Execute(%q): %v", cmd, err)
	}
	return out
}

// The sentinel is the only thing that tells the executor where a command's
// output ends and what directory it ended in, so the handshake has to establish
// both before the first command is allowed through.
func TestHandshakeEstablishesTheStartingCWD(t *testing.T) {
	bash := newFakeBash(nil)
	bash.cwd = "/workspace"

	exec := newTestExecutor(t, bash, time.Second)

	if cwd := exec.CWD(); cwd != "/workspace" {
		t.Errorf("CWD = %q, want /workspace", cwd)
	}
	if !exec.IsRunning() {
		t.Error("IsRunning = false on a freshly opened executor")
	}
	if opened := bash.sessionsOpened(); opened != 1 {
		t.Errorf("sessions opened = %d, want 1", opened)
	}
}

func TestHandshakeFailurePropagates(t *testing.T) {
	bash := newFakeBash(nil)
	bash.failOpenFrom = 1
	bash.openErr = errors.New("no such container")

	if _, err := newExecutor(context.Background(), bash, time.Second); err == nil {
		t.Fatal("newExecutor succeeded against a container that cannot be attached")
	}
}

func TestExecuteReturnsOutputAndTracksCWD(t *testing.T) {
	bash := newFakeBash(map[string]bashReply{
		"ls":            {output: "alpha\nbeta\n"},
		"cd sub":        {cwd: "/workspace/sub"},
		"cat notes.txt": {output: "hello\n"},
	})
	exec := newTestExecutor(t, bash, time.Second)

	if out, want := execute(t, exec, "ls"), withSentinelLead("alpha\nbeta\n"); out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if cwd := exec.CWD(); cwd != initialCWD {
		t.Errorf("CWD = %q, want it unchanged at %q", cwd, initialCWD)
	}

	execute(t, exec, "cd sub")
	if cwd := exec.CWD(); cwd != "/workspace/sub" {
		t.Errorf("CWD = %q, want the cd to have moved it", cwd)
	}

	// State persisting across commands is the whole reason the session is
	// long-lived; a second command must land in the directory the first left.
	if out, want := execute(t, exec, "cat notes.txt"), withSentinelLead("hello\n"); out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if opened := bash.sessionsOpened(); opened != 1 {
		t.Errorf("sessions opened = %d, want the one session reused", opened)
	}
}

// A command that never returns leaves bash owning stdin, so the session cannot
// be reused: the next command would sit unread behind the abandoned one and
// time out in its turn.
func TestTimeoutAbandonsTheSessionAndRecovers(t *testing.T) {
	bash := newFakeBash(map[string]bashReply{
		"sleep 300": {block: true},
		"echo back": {output: "back\n"},
	})
	bash.cwd = "/workspace/deep"
	exec := newTestExecutor(t, bash, 200*time.Millisecond)

	_, err := exec.Execute(context.Background(), "sleep 300")

	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want a *TimeoutError", err)
	}
	if timeoutErr.Command != "sleep 300" {
		t.Errorf("TimeoutError.Command = %q, want the command that hung", timeoutErr.Command)
	}
	if !strings.Contains(timeoutErr.Message, initialCWD) {
		t.Errorf("TimeoutError.Message = %q, want it to say where CWD reset to", timeoutErr.Message)
	}
	if opened := bash.sessionsOpened(); opened != 2 {
		t.Fatalf("sessions opened = %d, want the hung one abandoned and replaced", opened)
	}

	if out, want := execute(t, exec, "echo back"), withSentinelLead("back\n"); out != want {
		t.Errorf("output after recovery = %q, want %q", out, want)
	}
	if !exec.IsRunning() {
		t.Error("IsRunning = false after a recoverable timeout")
	}
}

// Cancellation reaches the executor when the operator stops a turn. It has to
// abandon the session for the same reason a timeout does, but it is not a
// timeout: reporting it as one would tell the model to raise command_timeout
// for something the operator did deliberately.
func TestCancelAbandonsTheSessionWithoutReportingATimeout(t *testing.T) {
	bash := newFakeBash(map[string]bashReply{
		"sleep 300": {block: true},
		"echo back": {output: "back\n"},
	})
	exec := newTestExecutor(t, bash, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := exec.Execute(ctx, "sleep 300")

	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want a cancellation rather than a timeout", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if opened := bash.sessionsOpened(); opened != 2 {
		t.Fatalf("sessions opened = %d, want the cancelled command's session abandoned", opened)
	}

	// The point of restarting: the next command must not queue behind the
	// abandoned one.
	if out, want := execute(t, exec, "echo back"), withSentinelLead("back\n"); out != want {
		t.Errorf("output after cancellation = %q, want %q", out, want)
	}
}

// When the restart itself fails there is no session left to drive, so the
// executor has to stop pretending it can run anything.
func TestFailedRestartClosesTheExecutor(t *testing.T) {
	bash := newFakeBash(map[string]bashReply{"sleep 300": {block: true}})
	bash.failOpenFrom = 2
	bash.openErr = errors.New("container is gone")
	exec := newTestExecutor(t, bash, 200*time.Millisecond)

	if _, err := exec.Execute(context.Background(), "sleep 300"); err == nil {
		t.Fatal("Execute succeeded on a command that never returned")
	}
	if exec.IsRunning() {
		t.Error("IsRunning = true after the session could not be reopened")
	}
	_, err := exec.Execute(context.Background(), "echo hello")
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("err = %v, want a fast failure naming the executor as closed", err)
	}
}

// The cap is a ceiling on what the host holds in memory. Exceeding it must not
// cost the model the fact that output went missing, or it will read a truncated
// result as the whole answer.
func TestOversizedOutputIsCappedAndAnnounced(t *testing.T) {
	line := strings.Repeat("x", 64<<10) + "\n"
	produced := maxCapturedBytes + 4<<20
	var flood strings.Builder
	for flood.Len() < produced {
		flood.WriteString(line)
	}
	bash := newFakeBash(map[string]bashReply{
		"cat huge.log": {output: flood.String()},
		"echo after":   {output: "after\n"},
	})
	exec := newTestExecutor(t, bash, 30*time.Second)

	out := execute(t, exec, "cat huge.log")

	if len(out) > maxCapturedBytes+1024 {
		t.Errorf("retained %d bytes, want it bounded near the %d-byte cap", len(out), maxCapturedBytes)
	}
	if !strings.Contains(out, "dropped as it arrived") {
		t.Errorf("output does not tell the model anything was dropped: %q", out[max(0, len(out)-300):])
	}

	// The tail of the oversized command still had to be consumed, or it would
	// be read back as this command's result.
	if after, want := execute(t, exec, "echo after"), withSentinelLead("after\n"); after != want {
		t.Errorf("next command returned %q, want %q", after, want)
	}
	if opened := bash.sessionsOpened(); opened != 1 {
		t.Errorf("sessions opened = %d, want the session to have survived", opened)
	}
}

// Execute is serialised so the TUI can issue overlapping calls; without the
// mutex two commands would interleave on one stdin and each would read part of
// the other's output. Meaningful under -race.
func TestConcurrentExecuteIsSerialised(t *testing.T) {
	replies := map[string]bashReply{}
	const workers = 8
	for i := 0; i < workers; i++ {
		replies[cmdN(i)] = bashReply{output: outN(i)}
	}
	bash := newFakeBash(replies)
	exec := newTestExecutor(t, bash, 10*time.Second)

	var wg sync.WaitGroup
	results := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := exec.Execute(context.Background(), cmdN(i))
			if err != nil {
				t.Errorf("Execute(%q): %v", cmdN(i), err)
				return
			}
			results[i] = out
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		if want := withSentinelLead(outN(i)); results[i] != want {
			t.Errorf("command %d got %q, want %q", i, results[i], want)
		}
	}
	if got := len(bash.executed()); got != workers {
		t.Errorf("commands that reached bash = %d, want %d", got, workers)
	}
}

func cmdN(i int) string { return "echo " + strings.Repeat("a", i+1) }
func outN(i int) string { return strings.Repeat("a", i+1) + "\n" }

func TestCloseIsIdempotentAndStopsFurtherCommands(t *testing.T) {
	bash := newFakeBash(map[string]bashReply{"echo hello": {output: "hello\n"}})
	exec := newTestExecutor(t, bash, time.Second)

	execute(t, exec, "echo hello")
	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := exec.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if exec.IsRunning() {
		t.Error("IsRunning = true after Close")
	}
	if _, err := exec.Execute(context.Background(), "echo hello"); err == nil {
		t.Error("Execute succeeded on a closed executor")
	}
}

func TestNewDockerExecutorRejectsAnIncompleteTarget(t *testing.T) {
	if _, err := NewDockerExecutor(context.Background(), nil, "abc", time.Second); err == nil {
		t.Error("NewDockerExecutor accepted a nil docker client")
	}
	if _, err := NewDockerExecutor(context.Background(), &client.Client{}, "", time.Second); err == nil {
		t.Error("NewDockerExecutor accepted an empty container id")
	}
}

// The TUI matches on the error type to offer raising command_timeout, and shows
// the message as written.
func TestTimeoutErrorReportsItsMessage(t *testing.T) {
	err := &TimeoutError{Command: "sleep 300", Duration: time.Minute, Message: "timed out after 1m0s"}
	if err.Error() != "timed out after 1m0s" {
		t.Errorf("Error() = %q, want the message it was built with", err.Error())
	}
}

// A container that dies mid-session breaks the write before anything can be
// read back, so the failure has to be reported rather than waited out for the
// full command timeout.
func TestWriteFailureIsReportedImmediately(t *testing.T) {
	exec := newTestExecutor(t, newFakeBash(nil), 30*time.Second)
	exec.sess.stdinW = brokenWriter{}

	start := time.Now()
	_, err := exec.Execute(context.Background(), "echo hello")

	if err == nil {
		t.Fatal("Execute succeeded with a broken connection to bash")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Execute took %s to report a write failure, want it to fail fast", elapsed)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

// Output that arrives after the executor stopped listening — a background job,
// or the tail of a command whose session was abandoned — must not be read back
// as the next command's result.
func TestDrainPendingDiscardsStaleLines(t *testing.T) {
	sess := &session{lineCh: make(chan readResult, 4)}
	sess.lineCh <- readResult{line: "stale from the last command\n"}
	sess.lineCh <- readResult{line: "and more\n"}

	sess.drainPending()

	if remaining := len(sess.lineCh); remaining != 0 {
		t.Errorf("%d stale lines left, want them all discarded", remaining)
	}
	sess.drainPending()
}
