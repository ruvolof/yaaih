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
			t.Errorf("%s (pid %d) is still running after the kill", name, pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A server left running keeps its port, so the agent's next attempt to start
// it would fail with the address in use: a kill has to reach everything the
// job started.
func TestKillStopsTheJobAndEverythingItStarted(t *testing.T) {
	executor := newLocalExecutor(t)
	dir := executor.CWD()
	result, err := executor.Start(context.Background(),
		"sleep 1000 & echo $! > background.pid\nbash -c 'echo $$ > foreground.pid; exec sleep 1000'", 200*time.Millisecond)
	if err != nil || !result.Running {
		t.Fatalf("Start = %+v, %v; want a running job", result, err)
	}

	killed, err := executor.Kill(context.Background(), result.ID)

	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if killed.Running {
		t.Error("Kill returned with the job still running")
	}
	waitUntilGone(t, "the foreground command", readPID(t, filepath.Join(dir, "foreground.pid")))
	waitUntilGone(t, "the background job", readPID(t, filepath.Join(dir, "background.pid")))
}

// Esc while the model waits on a command means stop it.
func TestCancellingStartKillsTheJob(t *testing.T) {
	executor := newLocalExecutor(t)
	dir := executor.CWD()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		readPID(t, filepath.Join(dir, "foreground.pid"))
		cancel()
	}()

	_, err := executor.Start(ctx, "bash -c 'echo $$ > foreground.pid; exec sleep 1000'", time.Minute)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	waitUntilGone(t, "the cancelled command", readPID(t, filepath.Join(dir, "foreground.pid")))
	if jobs := executor.Jobs(); len(jobs) != 1 || jobs[0].Running {
		t.Errorf("Jobs = %+v, want the cancelled job listed as finished", jobs)
	}
}

func TestCancellingWaitKillsTheJob(t *testing.T) {
	executor := newLocalExecutor(t)
	dir := executor.CWD()
	result, _ := executor.Start(context.Background(), "bash -c 'echo $$ > foreground.pid; exec sleep 1000'", 100*time.Millisecond)
	pid := readPID(t, filepath.Join(dir, "foreground.pid"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := executor.Wait(ctx, result.ID, time.Minute)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	waitUntilGone(t, "the job waited on", pid)
}

func TestCloseStopsRunningJobs(t *testing.T) {
	executor := newLocalExecutor(t)
	dir := executor.CWD()
	executor.Start(context.Background(), "bash -c 'echo $$ > foreground.pid; exec sleep 1000'", 0)
	pid := readPID(t, filepath.Join(dir, "foreground.pid"))

	executor.Close()

	waitUntilGone(t, "the running job", pid)
}

// Detaching with setsid is still the way to keep something running past a
// kill of the job that started it.
func TestDetachedProcessOutlivesItsJob(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on PATH")
	}
	executor := newLocalExecutor(t)
	dir := executor.CWD()
	result, _ := executor.Start(context.Background(),
		"setsid bash -c 'echo $$ > detached.pid; exec sleep 1000' >/dev/null 2>&1 &\nsleep 1000", 200*time.Millisecond)
	detachedPID := readPID(t, filepath.Join(dir, "detached.pid"))
	t.Cleanup(func() { _ = killLocalProcessGroup(detachedPID) })

	if _, err := executor.Kill(context.Background(), result.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	if !processAlive(detachedPID) {
		t.Error("the detached process was killed with its job")
	}
}

// A job that leaves a child holding its output still finishes when its shell
// does; the child's later output is its own business.
func TestBackgroundChildDoesNotHoldTheJobOpen(t *testing.T) {
	executor := newLocalExecutor(t)
	dir := executor.CWD()

	start := time.Now()
	out := run(t, executor, "bash -c 'echo $$ > child.pid; exec sleep 1000' &\necho started")

	t.Cleanup(func() { _ = syscall.Kill(readPID(t, filepath.Join(dir, "child.pid")), syscall.SIGKILL) })
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the job took %s to finish, want it done once its shell exits", elapsed)
	}
	if strings.TrimSpace(out) != "started" {
		t.Errorf("output = %q, want started", out)
	}
}
