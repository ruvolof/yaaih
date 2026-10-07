//go:build unix

package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startInOwnProcessGroup gives cmd the process group docker exec gives the
// container's bash, so the restart's kill reaches the same set of processes.
func startInOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killLocalProcessGroup(pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// readPID waits for a command running in the session to have written its pid.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && parseErr == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid written to %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A killed process lingers until it is reaped, so its absence is waited for.
func waitUntilGone(t *testing.T, name string, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Errorf("%s (pid %d) is still running after the restart", name, pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A server left running by a timed-out command keeps its port, so the agent's
// next attempt to start it fails with the address in use.
func TestTimeoutKillsWhatTheCommandStarted(t *testing.T) {
	exec := newLocalExecutor(t, 500*time.Millisecond)
	dir := exec.CWD()

	_, err := exec.Execute(context.Background(),
		"sleep 1000 & echo $! > background.pid\nbash -c 'echo $$ > foreground.pid; exec sleep 1000'")

	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want a *TimeoutError", err)
	}
	if !strings.Contains(timeoutErr.Message, "killed") {
		t.Errorf("TimeoutError.Message = %q, want it to say the command was killed", timeoutErr.Message)
	}
	waitUntilGone(t, "the foreground command", readPID(t, filepath.Join(dir, "foreground.pid")))
	waitUntilGone(t, "the background job", readPID(t, filepath.Join(dir, "background.pid")))
}

// A model that runs exit ends the shell. What it had started is still in the
// shell's process group and has to be killed with it, and the next command has
// to run in a fresh session. Docker ends the exec's stream once bash exits even
// while a child holds bash's stdout; a local pipe stays open, so the child here
// writes elsewhere.
func TestExitKillsWhatTheShellStartedAndRestarts(t *testing.T) {
	exec := newLocalExecutor(t, 30*time.Second)
	dir := exec.CWD()
	execute(t, exec, "sleep 1000 >/dev/null 2>&1 & echo $! > child.pid\nmkdir sub && cd sub")

	start := time.Now()
	_, err := exec.Execute(context.Background(), "exit")

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Execute took %s to notice bash exiting, want it at once", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "exited before this command finished") {
		t.Fatalf("err = %v, want it to say bash exited", err)
	}
	if !strings.Contains(err.Error(), "was killed") {
		t.Errorf("err = %q, want it to say what the old session left running was killed", err)
	}
	waitUntilGone(t, "the shell's child", readPID(t, filepath.Join(dir, "child.pid")))
	if out := execute(t, exec, "echo alive"); strings.TrimSpace(out) != "alive" {
		t.Errorf("output after the restart = %q, want alive", out)
	}
	if exec.CWD() != dir {
		t.Errorf("CWD = %q, want it back at the session's start, %q", exec.CWD(), dir)
	}
}

// A shell killed while idle, by the OOM killer say, has no command in flight to
// see it go. The next command must find out before it is sent rather than wait
// out its timeout, and must not run in a session it was not written for.
func TestShellKilledBetweenCommandsIsReplacedBeforeTheNextOne(t *testing.T) {
	exec := newLocalExecutor(t, 30*time.Second)
	dir := exec.CWD()
	execute(t, exec, "(sleep 0.2; kill -KILL $$; echo $$ > killed.pid) >/dev/null 2>&1 &")
	readPID(t, filepath.Join(dir, "killed.pid"))
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	_, err := exec.Execute(context.Background(), "touch ran")

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Execute took %s to notice bash was gone, want it at once", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "was not run") {
		t.Fatalf("err = %v, want it to say the command was not run", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ran")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the command ran anyway (stat err = %v)", statErr)
	}
	if out := execute(t, exec, "echo alive"); strings.TrimSpace(out) != "alive" {
		t.Errorf("output after the restart = %q, want alive", out)
	}
}

// The shell_exec description tells the model to start servers this way, so it
// has to hold: the job's output stays out of later results, and it survives the
// restart a later timeout causes.
func TestDetachedRedirectedJobOutlivesTheRestart(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on PATH")
	}
	exec := newLocalExecutor(t, 500*time.Millisecond)
	dir := exec.CWD()

	execute(t, exec, "setsid bash -c 'echo $$ > job.pid; while :; do echo tick; sleep 0.05; done' >job.log 2>&1 &")
	jobPID := readPID(t, filepath.Join(dir, "job.pid"))
	t.Cleanup(func() { _ = killLocalProcessGroup(jobPID) })

	if out := execute(t, exec, "sleep 0.3; echo done"); strings.TrimSpace(out) != "done" {
		t.Errorf("output = %q, want only the command's own", out)
	}
	var timeoutErr *TimeoutError
	if _, err := exec.Execute(context.Background(), "sleep 1000"); !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want a *TimeoutError", err)
	}

	if !processAlive(jobPID) {
		t.Fatal("the detached job was killed by the restart")
	}
	logBefore, _ := os.ReadFile(filepath.Join(dir, "job.log"))
	time.Sleep(200 * time.Millisecond)
	logAfter, _ := os.ReadFile(filepath.Join(dir, "job.log"))
	if len(logAfter) <= len(logBefore) {
		t.Errorf("job.log stopped growing at %d bytes, want the job still writing to it", len(logAfter))
	}
}
