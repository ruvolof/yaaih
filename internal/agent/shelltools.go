// The shell tools: running commands as jobs, and following, stopping, and
// listing the ones that outlast their call.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/executor"
	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/toolout"
)

// maxWaitSeconds caps how long one shell call may wait. A call that waits is a
// call the operator cannot see progress from, so a long job is better followed
// in several waits than in one.
const maxWaitSeconds = 600

// shellTools returns the shell tools, closing over runner. They are only
// registered when the runner exists.
func shellTools(cfg *config.Config, runner executor.Executor) []*Tool {
	defaultWait := cfg.Options.CommandWait
	waitSchema := func(purpose string) map[string]any {
		return map[string]any{
			"type":        "integer",
			"minimum":     0,
			"maximum":     maxWaitSeconds,
			"description": fmt.Sprintf("%s Defaults to %d; at most %d.", purpose, defaultWait, maxWaitSeconds),
		}
	}
	jobSchema := map[string]any{"type": "integer", "description": "The job's number, as shell_exec or shell_jobs reported it."}

	return []*Tool{
		{
			Capability: "shell",
			Restricted: true,
			Def: llm.ToolDefinition{
				Name: "shell_exec",
				Description: fmt.Sprintf(
					"Run a command in the persistent bash session inside the sandbox container. CWD, variables, functions, aliases, "+
						"and shell options persist from one call to the next. Output above %s is cut down before you see it — the first "+
						"and last part are kept and the full text is written to a file named in the result. Filtering in the command "+
						"itself (grep, head, tail, wc) gets you a better answer than reading the cut-down version of an unfiltered one.\n\n"+
						"Every command runs as a numbered job. The call waits for it up to wait_seconds (default %d). A command still "+
						"running then is not stopped: the call returns what it printed so far and the job keeps running in the "+
						"background, while you go on using the shell. What it changes in the session — cd, export — is kept only if "+
						"it finishes inside the call. To decide what to do with a running job, read the result: output still arriving "+
						"or processes busy on CPU means it is working, so shell_wait for it or do something else meanwhile; no output "+
						"for a long time with every process sleeping means it is probably stuck, so look at it (tail its log, ps, ss, "+
						"strace -p) and shell_kill it if it is. Pass a larger wait_seconds up front for a command you expect to be slow, "+
						"and 0 for a server or watcher meant to keep running.",
					toolout.HumanBytes(cfg.Options.MaxToolResultBytes), defaultWait),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "The command to execute. May span several lines, including heredocs; stdin is empty, so nothing can prompt for input.",
						},
						"wait_seconds": waitSchema("How long to wait for the command before leaving it running in the background."),
					},
					"required": []string{"command"},
				},
			},
			Fn: func(ctx context.Context, args string) (string, error) {
				var p struct {
					Command     string `json:"command"`
					WaitSeconds *int   `json:"wait_seconds"`
				}
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return "", fmt.Errorf("invalid args: %w", err)
				}
				if strings.TrimSpace(p.Command) == "" {
					return "", errors.New("command is empty")
				}
				result, err := runner.Start(ctx, p.Command, waitDuration(p.WaitSeconds, defaultWait))
				if err != nil {
					return result.Output, err
				}
				return describeStarted(result) + finishedNotices(runner), nil
			},
		},
		{
			Capability: "shell",
			Def: llm.ToolDefinition{
				Name: "shell_wait",
				Description: "Wait for a background job to finish, and get the output it printed since you last saw it. " +
					"Returns when the job ends or wait_seconds runs out, whichever comes first; the job keeps running either way.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"job":          jobSchema,
						"wait_seconds": waitSchema("How long to wait for the job to finish."),
					},
					"required": []string{"job"},
				},
			},
			Fn: func(ctx context.Context, args string) (string, error) {
				var p struct {
					Job         int  `json:"job"`
					WaitSeconds *int `json:"wait_seconds"`
				}
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return "", fmt.Errorf("invalid args: %w", err)
				}
				result, err := runner.Wait(ctx, p.Job, waitDuration(p.WaitSeconds, defaultWait))
				if err != nil {
					return result.Output, err
				}
				return describeFollowed(result) + finishedNotices(runner), nil
			},
		},
		{
			Capability: "shell",
			Restricted: true,
			Def: llm.ToolDefinition{
				Name:        "shell_kill",
				Description: "Stop a background job and every process it started, except ones detached with setsid.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"job": jobSchema},
					"required":   []string{"job"},
				},
			},
			Fn: func(ctx context.Context, args string) (string, error) {
				var p struct {
					Job int `json:"job"`
				}
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return "", fmt.Errorf("invalid args: %w", err)
				}
				result, err := runner.Kill(ctx, p.Job)
				if err != nil {
					return result.Output, err
				}
				return withOutput(result, fmt.Sprintf("[job %d stopped after %s]", result.ID, humanDuration(result.Elapsed))) +
					finishedNotices(runner), nil
			},
		},
		{
			Capability: "shell",
			Def: llm.ToolDefinition{
				Name:        "shell_jobs",
				Description: "List the background jobs that are running and the ones that finished recently.",
				Parameters:  emptyObjectSchema(),
			},
			Fn: func(ctx context.Context, args string) (string, error) {
				runner.TakeFinished()
				return describeJobs(runner.Jobs()), nil
			},
		},
	}
}

func waitDuration(requested *int, defaultSeconds int) time.Duration {
	seconds := defaultSeconds
	if requested != nil {
		seconds = *requested
	}
	return time.Duration(min(max(seconds, 0), maxWaitSeconds)) * time.Second
}

// describeStarted is shell_exec's result: the output, then what became of the
// command.
func describeStarted(result executor.JobResult) string {
	if result.Running {
		return withOutput(result, runningNote(result, true))
	}
	var notes []string
	switch {
	case result.ExitCode < 0:
		notes = append(notes, "[the shell running this command was killed before it could report how it ended]")
	case result.ExitCode != 0:
		notes = append(notes, fmt.Sprintf("[exit code %d]", result.ExitCode))
	}
	if !result.StateKept && result.ExitCode >= 0 {
		notes = append(notes, "[the shell's state after this command could not be saved — it replaced the shell with exec, or was killed — so its cd and export changes are lost]")
	}
	return withOutput(result, strings.Join(notes, "\n"))
}

// describeFollowed is shell_wait's result.
func describeFollowed(result executor.JobResult) string {
	if result.Running {
		return withOutput(result, runningNote(result, false))
	}
	return withOutput(result, "["+endOf(result.ID, result.ExitCode, result.Elapsed)+"]")
}

// runningNote is the status block of a job still running: everything the
// model needs to decide whether to wait, look closer, or stop it.
func runningNote(result executor.JobResult, justStarted bool) string {
	var note strings.Builder
	fmt.Fprintf(&note, "[job %d is still running after %s; it has not finished and was not stopped.\n", result.ID, humanDuration(result.Elapsed))
	if result.SinceOutput >= result.Elapsed {
		fmt.Fprintf(&note, " no output yet")
	} else {
		fmt.Fprintf(&note, " last output %s ago", humanDuration(result.SinceOutput))
	}
	if result.Processes != "" {
		note.WriteString(" · " + result.Processes)
	}
	note.WriteString("\n")
	if justStarted {
		note.WriteString(" its cd/export changes will not be kept\n")
	}
	fmt.Fprintf(&note, " log: %s · shell_wait(job=%d) for more · shell_kill(job=%d) to stop]", result.LogPath, result.ID, result.ID)
	return note.String()
}

// withOutput puts the job's output in front of note, saying first when some
// of it is no longer held and only the log has it.
func withOutput(result executor.JobResult, note string) string {
	var text strings.Builder
	if result.Skipped > 0 {
		fmt.Fprintf(&text, "[%s of output before this is not shown; the full output is in %s]\n",
			toolout.HumanBytes(int(result.Skipped)), result.LogPath)
	}
	text.WriteString(result.Output)
	if note != "" {
		if text.Len() > 0 && !strings.HasSuffix(text.String(), "\n") {
			text.WriteString("\n")
		}
		text.WriteString(note)
	}
	return text.String()
}

// finishedNotices tells the model, once, about background jobs that ended
// since it last heard from the shell, so it need not poll for them.
func finishedNotices(runner executor.Executor) string {
	var notices strings.Builder
	for _, job := range runner.TakeFinished() {
		fmt.Fprintf(&notices, "\n[background %s; log: %s]", endOf(job.ID, job.ExitCode, job.Elapsed), job.LogPath)
	}
	return notices.String()
}

func endOf(id, exitCode int, elapsed time.Duration) string {
	if exitCode < 0 {
		return fmt.Sprintf("job %d was killed after %s", id, humanDuration(elapsed))
	}
	return fmt.Sprintf("job %d finished: exit %d after %s", id, exitCode, humanDuration(elapsed))
}

func describeJobs(jobs []executor.JobInfo) string {
	if len(jobs) == 0 {
		return "No jobs."
	}
	var listing strings.Builder
	for _, job := range jobs {
		status := fmt.Sprintf("running for %s", humanDuration(job.Elapsed))
		switch {
		case job.Running:
		case job.ExitCode < 0:
			status = fmt.Sprintf("killed after %s", humanDuration(job.Elapsed))
		default:
			status = fmt.Sprintf("exit %d after %s", job.ExitCode, humanDuration(job.Elapsed))
		}
		command, _, multiline := strings.Cut(job.Command, "\n")
		if multiline {
			command += " …"
		}
		fmt.Fprintf(&listing, "job %d · %s · %s\n", job.ID, status, clip(command, 100))
	}
	fmt.Fprintf(&listing, "Logs: %s/<job>.log", path.Join(executor.ContainerWorkspace, executor.JobLogDir))
	return listing.String()
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
