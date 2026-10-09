// Package executor runs the model's shell commands inside the sandbox as jobs:
// one process per command, with its own output stream and exit code, that
// still behaves like a single persistent bash session.
//
// The session's state — working directory, variables, functions, aliases, and
// shell options — lives on the host as a snapshot. Every job starts a fresh
// bash, restores the snapshot, sources the command, and on its way out reports
// the state it ended with. A command that finishes inside its tool call
// commits that state as the new snapshot; a command still running when the
// call gives up is left running in the background, and nothing it changes is
// kept. That trade is what lets a command that hangs be investigated from the
// same session instead of killed with everything the session had built up.
//
// Concurrency model:
//
//   - Jobs run concurrently; nothing is serialised on a session. Each job is
//     supervised by its own goroutines: one copies its output into memory and
//     its log file, one reads the records the job shell reports.
//   - The snapshot is replaced only by a job that finished inside Start, so
//     two jobs can never race to commit: the agent calls Start one at a time,
//     and a backgrounded job forfeits its commit.
//   - Each job shell leads its own process group, and everything it starts
//     shares that group unless detached with setsid, so a kill takes the whole
//     job with it. The kill runs as a separate helper process: closing the
//     job's connection signals nothing.
package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/docker/docker/client"

	"github.com/ruvolof/yaaih/internal/logging"
)

var log = logging.For("executor")

// ContainerWorkspace is where the host workspace is bind-mounted inside the
// sandbox. A path handed to the model has to be expressed against this root:
// the host path the harness writes to does not exist in the container the model
// runs its commands in.
const ContainerWorkspace = "/workspace"

// JobLogDir is where every job's full output is written, relative to the
// workspace root, so the model and the operator can tail it from inside the
// sandbox while the job runs.
const JobLogDir = ".harness/jobs"

// maxCapturedBytes bounds how much of one job's output the harness holds in
// memory. It is a ceiling against `cat` of a huge file or a chatty server, not
// a context budget — what the model receives is bounded far lower, further up
// the stack. The newest output is the part kept: the start of it is in the log.
const maxCapturedBytes = 8 << 20

// MaxRunningJobs caps the jobs running at once. Every one of them holds
// processes against the container's PID limit, and a model that forgets its
// jobs would otherwise find out only when fork starts failing.
const MaxRunningJobs = 8

// retainedFinishedJobs is how many finished jobs stay listed. Older ones are
// forgotten once their final status has been reported.
const retainedFinishedJobs = 32

// retainedLogBytes is the total size of job logs kept on disk. Logs of
// finished jobs go oldest first once the directory exceeds it.
const retainedLogBytes = 64 << 20

// historyLimit bounds the commands remembered for the operator's shell.
const historyLimit = 1000

// killWait is how long a kill waits for the job to report its end.
const killWait = 5 * time.Second

// helperTimeout bounds the short helper processes: kills and process listings.
const helperTimeout = 5 * time.Second

// Runner runs harness-internal commands: ones the model did not write, such as
// the file tools' and script tools'. They see the session's state but never
// change it, and they are killed rather than backgrounded when ctx ends.
type Runner interface {
	Run(ctx context.Context, cmd string) (JobResult, error)
}

// Executor is the contract used by the shell tools.
type Executor interface {
	Runner
	// Start runs cmd as a job and waits up to wait for it to finish. A job
	// still running then keeps running in the background, and whatever it
	// changes in the session's state is not kept. Cancelling ctx while it
	// waits kills the job.
	Start(ctx context.Context, cmd string, wait time.Duration) (JobResult, error)
	// Wait waits up to wait for job id to finish and returns the output it
	// produced since it was last read. Cancelling ctx kills the job.
	Wait(ctx context.Context, id int, wait time.Duration) (JobResult, error)
	// Kill stops job id and everything it started.
	Kill(ctx context.Context, id int) (JobResult, error)
	// Jobs lists the running jobs and the recently finished ones, oldest first.
	Jobs() []JobInfo
	// TakeFinished returns the jobs that finished in the background since the
	// last call and whose end nobody has been told about yet.
	TakeFinished() []JobInfo
	CWD() string
	IsRunning() bool
	Close() error
}

// JobResult is what one Start, Wait, Kill, or Run call saw of a job.
type JobResult struct {
	ID int
	// Output is the job's output since it was last read.
	Output string
	// Skipped counts output that arrived since the last read but is no longer
	// held in memory; the log has all of it.
	Skipped int64
	// Running is true when the job had not finished by the time the call
	// returned.
	Running bool
	// ExitCode is the job's exit status once it has finished; -1 when the job
	// shell was killed before it could report one.
	ExitCode int
	// StateKept reports whether a job that finished inside Start had its
	// state saved as the session's.
	StateKept bool
	Elapsed   time.Duration
	// SinceOutput is how long ago the job last wrote anything, or how long it
	// has gone without writing anything at all.
	SinceOutput time.Duration
	// Processes describes what a running job's process group is doing, when
	// it could be listed.
	Processes string
	// LogPath is the job's full output as a path inside the container; empty
	// for harness-internal runs, which keep no log.
	LogPath string
}

// JobInfo describes one job for listings.
type JobInfo struct {
	ID       int
	Command  string
	Running  bool
	ExitCode int
	Elapsed  time.Duration
	LogPath  string
}

// HistoryEntry is one command the model ran, for the operator's shell.
type HistoryEntry struct {
	Command string
	Started time.Time
}

// JobExecutor runs jobs through a shellHost and keeps the session's state
// between them.
type JobExecutor struct {
	host       shellHost
	hostLogDir string

	mu       sync.Mutex
	closed   bool
	state    *shellState
	nextID   int
	jobs     map[int]*job
	finished []int // tracked jobs in the order they finished
	history  []HistoryEntry
}

// NewDockerExecutor captures the container's initial shell state and returns
// an Executor running jobs in it. workspace is the host directory bind-mounted
// at ContainerWorkspace, where job logs are written. The caller is responsible
// for keeping the container alive for the executor's lifetime.
func NewDockerExecutor(ctx context.Context, cli *client.Client, containerID, workspace string) (*JobExecutor, error) {
	if cli == nil {
		return nil, errors.New("executor: nil docker client")
	}
	if containerID == "" {
		return nil, errors.New("executor: empty container id")
	}

	e, err := newExecutor(ctx, dockerHost{cli: cli, containerID: containerID}, workspace)
	if err != nil {
		return nil, err
	}
	log.Info("shell ready", "container_id", containerID[:12], "cwd", e.CWD())
	return e, nil
}

// newExecutor runs the baseline job: an empty command started without a
// snapshot, whose saved state is the container's own environment.
func newExecutor(ctx context.Context, host shellHost, workspace string) (*JobExecutor, error) {
	hostLogDir := filepath.Join(workspace, filepath.FromSlash(JobLogDir))
	if err := os.MkdirAll(hostLogDir, 0o755); err != nil {
		return nil, fmt.Errorf("executor: create job log directory: %w", err)
	}
	e := &JobExecutor{
		host:       host,
		hostLogDir: hostLogDir,
		nextID:     nextJobID(hostLogDir),
		jobs:       map[int]*job{},
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	baseline, err := e.launch(initCtx, 0, ":", nil, false)
	if err != nil {
		return nil, fmt.Errorf("executor: start the baseline shell: %w", err)
	}
	select {
	case <-baseline.done:
	case <-initCtx.Done():
		e.killJob(baseline)
		return nil, fmt.Errorf("executor: the baseline shell did not report its state: %w", initCtx.Err())
	}
	baseline.mu.Lock()
	state, output := baseline.state, string(baseline.window)
	baseline.mu.Unlock()
	if state == nil {
		return nil, fmt.Errorf("executor: the baseline shell ended without reporting its state: %s", truncate(strings.TrimSpace(output), 200))
	}
	e.state = state
	return e, nil
}

// nextJobID continues numbering after the logs already in dir, so a resumed
// session's job ids, and the log files named after them, never collide with an
// earlier run's.
func nextJobID(dir string) int {
	highest := 0
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if id, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".log")); err == nil && id > highest {
			highest = id
		}
	}
	return highest + 1
}

func (e *JobExecutor) Start(ctx context.Context, cmd string, wait time.Duration) (JobResult, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return JobResult{}, errors.New("executor: closed")
	}
	if running := e.runningIDsLocked(); len(running) >= MaxRunningJobs {
		e.mu.Unlock()
		return JobResult{}, fmt.Errorf("%d jobs are already running (%s); wait for one to finish or stop one with shell_kill before starting another",
			len(running), joinIDs(running))
	}
	id := e.nextID
	e.nextID++
	snapshot := e.state
	e.history = append(e.history, HistoryEntry{Command: cmd, Started: time.Now()})
	if len(e.history) > historyLimit {
		e.history = e.history[len(e.history)-historyLimit:]
	}
	e.mu.Unlock()

	e.pruneLogs()
	j, err := e.launch(ctx, id, cmd, snapshot, true)
	if err != nil {
		return JobResult{}, err
	}
	log.Debug("job started", "job", id, "cmd", truncate(cmd, 200), "cwd", snapshot.cwd)

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
	case <-ctx.Done():
		e.killJob(j)
		e.markReported(j)
		return j.read(true), fmt.Errorf("executor: command cancelled: %w", ctx.Err())
	}

	j.mu.Lock()
	finished := j.finished
	if finished {
		j.reported = true
	} else {
		j.backgrounded = true
	}
	state := j.state
	j.mu.Unlock()

	result := j.read(true)
	if !finished {
		result.Processes = e.processes(j)
		log.Info("job backgrounded", "job", id, "cmd", truncate(cmd, 100), "after", wait.String())
		return result, nil
	}

	if state != nil {
		e.mu.Lock()
		e.state = state
		e.mu.Unlock()
		result.StateKept = true
	}
	log.Info("job finished", "job", id, "cmd", truncate(cmd, 100), "exit", result.ExitCode,
		"out_bytes", len(result.Output), "elapsed_ms", result.Elapsed.Milliseconds(), "state_kept", result.StateKept)
	return result, nil
}

func (e *JobExecutor) Run(ctx context.Context, cmd string) (JobResult, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return JobResult{}, errors.New("executor: closed")
	}
	snapshot := e.state
	e.mu.Unlock()

	j, err := e.launch(ctx, 0, cmd, snapshot, false)
	if err != nil {
		return JobResult{}, err
	}
	select {
	case <-j.done:
		return j.read(false), nil
	case <-ctx.Done():
		e.killJob(j)
		return j.read(false), fmt.Errorf("executor: command stopped: %w", ctx.Err())
	}
}

func (e *JobExecutor) Wait(ctx context.Context, id int, wait time.Duration) (JobResult, error) {
	j, err := e.lookup(id)
	if err != nil {
		return JobResult{}, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
	case <-ctx.Done():
		e.killJob(j)
		result := j.read(true)
		e.markReported(j)
		return result, fmt.Errorf("executor: wait cancelled, job %d stopped: %w", id, ctx.Err())
	}
	result := j.read(true)
	if result.Running {
		result.Processes = e.processes(j)
	} else {
		e.markReported(j)
	}
	return result, nil
}

func (e *JobExecutor) Kill(ctx context.Context, id int) (JobResult, error) {
	j, err := e.lookup(id)
	if err != nil {
		return JobResult{}, err
	}
	if !e.killJob(j) {
		return j.read(true), fmt.Errorf("job %d could not be stopped and may still be running", id)
	}
	e.markReported(j)
	return j.read(true), nil
}

func (e *JobExecutor) Jobs() []JobInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]int, 0, len(e.jobs))
	for id := range e.jobs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	infos := make([]JobInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, e.jobs[id].info())
	}
	return infos
}

func (e *JobExecutor) TakeFinished() []JobInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	var infos []JobInfo
	for _, id := range e.finished {
		j := e.jobs[id]
		j.mu.Lock()
		unreported := j.backgrounded && !j.reported
		j.reported = true
		j.mu.Unlock()
		if unreported {
			infos = append(infos, j.info())
		}
	}
	return infos
}

// History returns the commands the model ran, oldest first.
func (e *JobExecutor) History() []HistoryEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]HistoryEntry(nil), e.history...)
}

// RunningJobs counts the jobs still running.
func (e *JobExecutor) RunningJobs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.runningIDsLocked())
}

func (e *JobExecutor) CWD() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == nil {
		return ContainerWorkspace
	}
	return e.state.cwd
}

func (e *JobExecutor) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed
}

// Close stops every running job. The container usually goes away right after,
// but a job outliving its executor would otherwise keep running unwatched.
func (e *JobExecutor) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	var running []*job
	for _, id := range e.runningIDsLocked() {
		running = append(running, e.jobs[id])
	}
	e.mu.Unlock()

	var wg sync.WaitGroup
	for _, j := range running {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.killJob(j)
		}()
	}
	wg.Wait()
	return nil
}

// launch starts one job. tracked jobs are the model's: they get an id, a log
// file, and a place in the listings. snapshot nil starts the shell as the
// container gives it, which only the baseline job wants.
func (e *JobExecutor) launch(ctx context.Context, id int, cmd string, snapshot *shellState, tracked bool) (*job, error) {
	delimiter, err := newDelimiter()
	if err != nil {
		return nil, fmt.Errorf("executor: generate heredoc delimiter: %w", err)
	}
	j := newJob(id, cmd)
	if tracked {
		j.onFinish = func() { e.retire(j) }
		j.logPath = path.Join(ContainerWorkspace, JobLogDir, strconv.Itoa(id)+".log")
		logFile, err := os.Create(filepath.Join(e.hostLogDir, strconv.Itoa(id)+".log"))
		if err != nil {
			log.Warn("job log unavailable", "job", id, "err", err)
		} else {
			j.logFile = logFile
		}
	}
	proc, err := e.host.startJob(ctx, jobScript(cmd, snapshot, delimiter))
	if err != nil {
		if j.logFile != nil {
			j.logFile.Close()
		}
		return nil, err
	}
	if tracked {
		e.mu.Lock()
		e.jobs[id] = j
		e.mu.Unlock()
	}
	go j.supervise(proc)
	return j, nil
}

// killJob stops the job's process group and waits for its end to be reported.
// It reports false when the job was still running when the wait gave up.
func (e *JobExecutor) killJob(j *job) bool {
	select {
	case <-j.done:
		return true
	default:
	}

	pgid := j.awaitPGID(time.Second)
	if pgid > 0 {
		killCtx, cancel := context.WithTimeout(context.Background(), helperTimeout)
		output, err := e.host.runHelper(killCtx, killScript(pgid))
		cancel()
		if err != nil || strings.TrimSpace(output) != "stopped" {
			log.Warn("could not kill the job's processes", "job", j.id, "pgid", pgid, "err", err, "output", truncate(output, 200))
		}
	}
	select {
	case <-j.done:
		return true
	case <-time.After(killWait):
		// Without a pid there was nothing to signal, and a job shell that
		// will not report its end is abandoned: ending its stream at least
		// releases the supervisor.
		j.abandon()
		log.Warn("job did not end after the kill", "job", j.id, "pgid", pgid)
		return false
	}
}

// processes lists what a running job's process group is doing. It is a hint
// for the model, so a failure only leaves it out.
func (e *JobExecutor) processes(j *job) string {
	pgid := j.awaitPGID(0)
	if pgid <= 0 {
		return ""
	}
	listCtx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	output, err := e.host.runHelper(listCtx, processListScript)
	if err != nil {
		log.Debug("process listing failed", "job", j.id, "err", err)
		return ""
	}
	return describeProcessGroup(output, pgid)
}

func (e *JobExecutor) lookup(id int) (*job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok {
		return nil, fmt.Errorf("there is no job %d; shell_jobs lists the ones there are", id)
	}
	return j, nil
}

// markReported records that the model has seen the job's end.
func (e *JobExecutor) markReported(j *job) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished {
		j.reported = true
	}
}

// retire moves a finished job to the finished list, forgetting the oldest ones
// whose end has been reported once the list is full. Every tracked job's
// supervisor calls it as the job ends.
func (e *JobExecutor) retire(j *job) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, tracked := e.jobs[j.id]; !tracked {
		return
	}
	for _, id := range e.finished {
		if id == j.id {
			return
		}
	}
	e.finished = append(e.finished, j.id)
	for len(e.finished) > retainedFinishedJobs {
		oldest := e.jobs[e.finished[0]]
		oldest.mu.Lock()
		reported := oldest.reported
		oldest.mu.Unlock()
		if !reported {
			break
		}
		delete(e.jobs, oldest.id)
		e.finished = e.finished[1:]
	}
}

// runningIDsLocked lists the tracked jobs that have not finished. Caller must
// hold e.mu.
func (e *JobExecutor) runningIDsLocked() []int {
	var ids []int
	for id, j := range e.jobs {
		select {
		case <-j.done:
		default:
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// pruneLogs deletes the logs of finished jobs, oldest first, until the log
// directory fits in retainedLogBytes. A running job's log is never touched.
// Failures are logged and otherwise ignored: an old log is not worth failing
// a command over.
func (e *JobExecutor) pruneLogs() {
	entries, err := os.ReadDir(e.hostLogDir)
	if err != nil {
		log.Warn("job log prune failed to list directory", "err", err, "dir", e.hostLogDir)
		return
	}
	type jobLog struct {
		id    int
		bytes int64
	}
	var logs []jobLog
	var total int64
	for _, entry := range entries {
		id, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".log"))
		info, infoErr := entry.Info()
		if err != nil || infoErr != nil || entry.IsDir() {
			continue
		}
		logs = append(logs, jobLog{id: id, bytes: info.Size()})
		total += info.Size()
	}
	if total <= retainedLogBytes {
		return
	}
	e.mu.Lock()
	running := map[int]bool{}
	for _, id := range e.runningIDsLocked() {
		running[id] = true
	}
	e.mu.Unlock()

	sort.Slice(logs, func(a, b int) bool { return logs[a].id < logs[b].id })
	for _, jobLog := range logs {
		if total <= retainedLogBytes {
			return
		}
		if running[jobLog.id] {
			continue
		}
		if err := os.Remove(filepath.Join(e.hostLogDir, strconv.Itoa(jobLog.id)+".log")); err != nil {
			log.Warn("job log prune failed", "err", err, "job", jobLog.id)
			continue
		}
		total -= jobLog.bytes
	}
}

func joinIDs(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ", ")
}

// truncate is a small helper for log fields so we don't dump multi-KB
// commands or outputs into the log file by accident. n is a byte budget; the
// cut steps back to the start of a rune so the log never holds half of one.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
