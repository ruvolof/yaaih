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
