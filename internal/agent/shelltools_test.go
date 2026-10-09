package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/executor"
)

// jobRunner answers every shell call with canned results and records the wait
// each call asked for.
type jobRunner struct {
	stubRunner
	started  executor.JobResult
	waited   executor.JobResult
	finished []executor.JobInfo
	waits    []time.Duration
}

func (r *jobRunner) Start(ctx context.Context, cmd string, wait time.Duration) (executor.JobResult, error) {
	r.waits = append(r.waits, wait)
	return r.started, nil
}

func (r *jobRunner) Wait(ctx context.Context, id int, wait time.Duration) (executor.JobResult, error) {
	r.waits = append(r.waits, wait)
	return r.waited, nil
}

func (r *jobRunner) TakeFinished() []executor.JobInfo {
	finished := r.finished
	r.finished = nil
	return finished
}

func callShellTool(t *testing.T, runner executor.Executor, name, args string) string {
	t.Helper()
	cfg := &config.Config{Options: config.Options{CommandWait: 60, MaxToolResultBytes: 1 << 10}}
	for _, tool := range shellTools(cfg, runner) {
		if tool.Def.Name == name {
			result, err := tool.Fn(context.Background(), args)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return result
		}
	}
	t.Fatalf("no shell tool %q", name)
	return ""
}

func TestRunningJobResultSaysItIsAliveAndHowToFollowIt(t *testing.T) {
	runner := &jobRunner{started: executor.JobResult{
		ID: 4, Output: "compiling\n", Running: true, Elapsed: time.Minute, SinceOutput: 2 * time.Second,
		Processes: "1 process: node (running, 97% cpu)", LogPath: "/workspace/.harness/jobs/4.log",
	}}

	result := callShellTool(t, runner, "shell_exec", `{"command":"npm run build"}`)

	for _, want := range []string{
		"compiling\n[job 4 is still running after 1m0s; it has not finished and was not stopped.",
		"last output 2s ago · 1 process: node (running, 97% cpu)",
		"its cd/export changes will not be kept",
		"log: /workspace/.harness/jobs/4.log · shell_wait(job=4) for more · shell_kill(job=4) to stop]",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("result = %q, want it to contain %q", result, want)
		}
	}
}

func TestFinishedJobResultReportsOnlyWhatIsUnusual(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result executor.JobResult
		want   string
	}{
		{"success", executor.JobResult{Output: "ok\n", StateKept: true}, "ok\n"},
		{"failure", executor.JobResult{Output: "no\n", ExitCode: 2, StateKept: true}, "no\n[exit code 2]"},
		{"state lost", executor.JobResult{Output: "ok\n"}, "ok\n[the shell's state after this command could not be saved"},
		{"killed", executor.JobResult{ExitCode: -1}, "[the shell running this command was killed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := callShellTool(t, &jobRunner{started: tc.result}, "shell_exec", `{"command":"x"}`)
			if !strings.HasPrefix(result, tc.want) {
				t.Errorf("result = %q, want it to start with %q", result, tc.want)
			}
		})
	}
}

func TestWaitSecondsDefaultsAndIsClamped(t *testing.T) {
	runner := &jobRunner{}

	callShellTool(t, runner, "shell_exec", `{"command":"x"}`)
	callShellTool(t, runner, "shell_exec", `{"command":"x","wait_seconds":0}`)
	callShellTool(t, runner, "shell_exec", `{"command":"x","wait_seconds":100000}`)
	callShellTool(t, runner, "shell_wait", `{"job":1,"wait_seconds":-5}`)

	want := []time.Duration{time.Minute, 0, maxWaitSeconds * time.Second, 0}
	if len(runner.waits) != len(want) {
		t.Fatalf("waits = %v, want %v", runner.waits, want)
	}
	for i := range want {
		if runner.waits[i] != want[i] {
			t.Errorf("waits = %v, want %v", runner.waits, want)
			break
		}
	}
}

// A job that ended in the background is mentioned on the next shell result,
// so the model hears of it without polling.
func TestBackgroundEndsRideOnTheNextShellResult(t *testing.T) {
	runner := &jobRunner{
		started:  executor.JobResult{Output: "hi\n", StateKept: true},
		finished: []executor.JobInfo{{ID: 3, ExitCode: 0, Elapsed: 3 * time.Minute, LogPath: "/workspace/.harness/jobs/3.log"}},
	}

	result := callShellTool(t, runner, "shell_exec", `{"command":"echo hi"}`)

	if want := "hi\n\n[background job 3 finished: exit 0 after 3m0s; log: /workspace/.harness/jobs/3.log]"; result != want {
		t.Errorf("result = %q, want %q", result, want)
	}
}

func TestShellWaitReportsTheEnd(t *testing.T) {
	runner := &jobRunner{waited: executor.JobResult{ID: 3, Output: "done\n", Elapsed: 90 * time.Second}}

	if result := callShellTool(t, runner, "shell_wait", `{"job":3}`); result != "done\n[job 3 finished: exit 0 after 1m30s]" {
		t.Errorf("result = %q", result)
	}
}

func TestShellJobsListsEachJobOnALine(t *testing.T) {
	runner := &jobRunner{}
	jobs := []executor.JobInfo{
		{ID: 1, Command: "make\nmake install", ExitCode: 0, Elapsed: 5 * time.Second},
		{ID: 2, Command: "python3 -m http.server", Running: true, Elapsed: time.Minute},
	}

	listing := describeJobs(jobs)

	for _, want := range []string{
		"job 1 · exit 0 after 5s · make …\n",
		"job 2 · running for 1m0s · python3 -m http.server\n",
		"Logs: /workspace/.harness/jobs/<job>.log",
	} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing = %q, want it to contain %q", listing, want)
		}
	}
	if got := callShellTool(t, runner, "shell_jobs", `{}`); got != "No jobs." {
		t.Errorf("empty listing = %q", got)
	}
}
