package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// localHost runs job shells with a bash on the test host, for the behaviour
// only a real shell can show: what state survives from one job to the next,
// how syntax errors, stdin, and exit are handled. Like the container's, each
// job shell leads its own process group.
type localHost struct {
	dir string
}

// baselineVariable is in every local job shell's environment, the way the
// container's environment reaches every exec.
const baselineVariable = "YAAIH_TEST_BASELINE"

func (l localHost) startJob(ctx context.Context, script string) (*jobProcess, error) {
	cmd := exec.Command("bash", "--norc", "--noprofile", "-c", jobBootstrap)
	cmd.Dir = l.dir
	cmd.Env = append(os.Environ(), baselineVariable+"=present")
	cmd.Stdin = strings.NewReader(script)
	startInOwnProcessGroup(cmd)
	outputR, outputW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	metaR, metaW, err := os.Pipe()
	if err != nil {
		outputR.Close()
		outputW.Close()
		return nil, err
	}
	cmd.Stdout = outputW
	cmd.Stderr = metaW
	if err := cmd.Start(); err != nil {
		for _, pipeEnd := range []*os.File{outputR, outputW, metaR, metaW} {
			pipeEnd.Close()
		}
		return nil, err
	}
	outputW.Close()
	metaW.Close()
	go func() { _ = cmd.Wait() }()

	return &jobProcess{
		output: outputR,
		meta:   metaR,
		close: func() {
			outputR.Close()
			metaR.Close()
		},
	}, nil
}

func (l localHost) runHelper(ctx context.Context, script string) (string, error) {
	output, err := exec.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
	return string(output), err
}

func newLocalExecutor(t *testing.T) *JobExecutor {
	t.Helper()
	for _, tool := range []string{"bash", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH", tool)
		}
	}
	dir := t.TempDir()
	executor, err := newExecutor(context.Background(), localHost{dir: dir}, dir)
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	t.Cleanup(func() { executor.Close() })
	return executor
}

// run starts cmd and expects it to finish inside the call with exit status 0.
func run(t *testing.T, executor *JobExecutor, cmd string) string {
	t.Helper()
	result, err := executor.Start(context.Background(), cmd, 10*time.Second)
	if err != nil {
		t.Fatalf("Start(%q): %v", cmd, err)
	}
	if result.Running || result.ExitCode != 0 {
		t.Fatalf("Start(%q) = running %v, exit %d, output %q; want it finished with 0", cmd, result.Running, result.ExitCode, result.Output)
	}
	return result.Output
}

func TestBaselineStartsInTheWorkspace(t *testing.T) {
	executor := newLocalExecutor(t)

	if out := run(t, executor, "pwd"); strings.TrimSpace(out) != executor.CWD() {
		t.Errorf("pwd = %q, want the CWD the executor reports, %q", out, executor.CWD())
	}
	if !executor.IsRunning() {
		t.Error("IsRunning = false on a freshly opened executor")
	}
}

// Every job is a fresh process, so everything a session carries has to make
// the trip through the snapshot.
func TestSessionStatePersistsFromJobToJob(t *testing.T) {
	executor := newLocalExecutor(t)

	run(t, executor, strings.Join([]string{
		"mkdir -p nested/dir && cd nested/dir",
		"export GREETING=hi",
		"plain='not exported'",
		"multiline=$'one\\ntwo'",
		"declare -a list=(first 'second item')",
		"declare -A table=([key]=value)",
		"greet() { echo \"hello $1\"; }",
		"shopt -s expand_aliases extglob",
		"alias say='echo said'",
		"set -o pipefail",
	}, "\n"))

	if !strings.HasSuffix(executor.CWD(), "/nested/dir") {
		t.Errorf("CWD = %q, want the cd to persist", executor.CWD())
	}
	for _, check := range []struct{ command, want string }{
		{"pwd", executor.CWD()},
		{`echo "$GREETING"`, "hi"},
		{`bash -c 'echo "$GREETING"'`, "hi"},
		{`echo "$plain"`, "not exported"},
		{`bash -c 'echo "${plain-unset in children}"'`, "unset in children"},
		{`echo "$multiline"`, "one\ntwo"},
		{`echo "${list[1]}"`, "second item"},
		{`echo "${table[key]}"`, "value"},
		{"greet you", "hello you"},
		{"say it", "said it"},
		{"touch keep.txt drop.log\nls !(*.log)", "keep.txt"},
		{"false | true; echo $?", "1"},
	} {
		if out := run(t, executor, check.command); strings.TrimSpace(out) != check.want {
			t.Errorf("%s: output = %q, want %q", check.command, out, check.want)
		}
	}
}

// Each job starts from the container's environment, so an unset is only
// honoured if the snapshot replaces that environment rather than adding to it.
func TestUnsetOfAnInheritedVariablePersists(t *testing.T) {
	executor := newLocalExecutor(t)
	if out := run(t, executor, "echo ${"+baselineVariable+"-gone}"); strings.TrimSpace(out) != "present" {
		t.Fatalf("baseline variable = %q, want it inherited", out)
	}

	run(t, executor, "unset "+baselineVariable)

	if out := run(t, executor, "echo ${"+baselineVariable+"-gone}"); strings.TrimSpace(out) != "gone" {
		t.Errorf("after unset = %q, want it to stay unset", out)
	}
}

// The wrapper's own variables must not be ones a model is likely to use.
func TestSessionVariablesSurviveTheWrapper(t *testing.T) {
	executor := newLocalExecutor(t)
	run(t, executor, "f=mine\nfile=also-mine\ncommand=mine-too")

	if out := run(t, executor, `echo "$f $file $command"`); strings.TrimSpace(out) != "mine also-mine mine-too" {
		t.Errorf("output = %q, want the session's own variables intact", out)
	}
}

// In a single long-lived bash, exit ended the session. A job's exit ends only
// the job, and what it did before exiting is kept.
func TestExitEndsOnlyTheJobAndKeepsItsState(t *testing.T) {
	executor := newLocalExecutor(t)

	result, err := executor.Start(context.Background(), "mkdir sub && cd sub\nexport KEPT=yes\necho bye\nexit 3", 10*time.Second)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.ExitCode != 3 || !result.StateKept {
		t.Errorf("exit %d, state kept %v; want 3 and kept", result.ExitCode, result.StateKept)
	}
	if strings.TrimSpace(result.Output) != "bye" {
		t.Errorf("output = %q, want bye", result.Output)
	}
	if out := run(t, executor, `echo "$KEPT"; pwd`); out != "yes\n"+executor.CWD()+"\n" || !strings.HasSuffix(executor.CWD(), "/sub") {
		t.Errorf("after exit: output %q, CWD %q; want the export and the cd kept", out, executor.CWD())
	}
}

func TestErrexitAndNounsetPersistWithoutEndingTheSession(t *testing.T) {
	executor := newLocalExecutor(t)
	run(t, executor, "set -eu")

	result, err := executor.Start(context.Background(), "false\necho after", 10*time.Second)
	if err != nil || result.ExitCode != 1 || strings.Contains(result.Output, "after") {
		t.Errorf("under errexit: exit %d, output %q, err %v; want it to stop at false with 1", result.ExitCode, result.Output, err)
	}
	result, err = executor.Start(context.Background(), `echo "$undefined_variable"`, 10*time.Second)
	if err != nil || result.ExitCode == 0 || !strings.Contains(result.Output, "unbound variable") {
		t.Errorf("under nounset: exit %d, output %q, err %v; want an unbound variable error", result.ExitCode, result.Output, err)
	}
	run(t, executor, "set +eu")
	if out := run(t, executor, "false; echo ${still_undefined-fine}"); strings.TrimSpace(out) != "fine" {
		t.Errorf("after set +eu: output %q, want both options off", out)
	}
}

// A session's `set -x` should trace its commands, not the harness's.
func TestXtraceShowsOnlyTheCommand(t *testing.T) {
	executor := newLocalExecutor(t)
	run(t, executor, "set -x")

	out := run(t, executor, "echo traced")

	if !strings.Contains(out, "+ echo traced") {
		t.Errorf("output = %q, want the command traced", out)
	}
	for _, internal := range []string{"__yaaih", "shopt", "set -o", "set +o"} {
		if strings.Contains(out, internal) {
			t.Errorf("output = %q, want nothing of the harness traced (found %q)", out, internal)
		}
	}
}

// A command's own EXIT trap replaces the job shell's, so the state has to be
// saved without it.
func TestCommandSettingAnExitTrapKeepsItsState(t *testing.T) {
	executor := newLocalExecutor(t)

	result, err := executor.Start(context.Background(), "trap 'echo trap ran' EXIT\nmkdir sub && cd sub", 10*time.Second)

	if err != nil || !result.StateKept {
		t.Fatalf("state kept %v, err %v; want it kept", result.StateKept, err)
	}
	if !strings.HasSuffix(executor.CWD(), "/sub") {
		t.Errorf("CWD = %q, want the cd kept", executor.CWD())
	}
	if !strings.Contains(result.Output, "trap ran") {
		t.Errorf("output = %q, want the command's own trap to have run", result.Output)
	}
}

func TestHeredocRuns(t *testing.T) {
	executor := newLocalExecutor(t)

	if out := run(t, executor, "cat <<'EOF'\nhello\nEOF"); strings.TrimSpace(out) != "hello" {
		t.Errorf("output = %q, want hello", out)
	}
}

func TestCommandReadingStdinGetsEndOfInput(t *testing.T) {
	executor := newLocalExecutor(t)

	out := run(t, executor, "cat; read -r answer; echo \"read status $?\"")

	if strings.TrimSpace(out) != "read status 1" {
		t.Errorf("output = %q, want cat and read to see an empty stdin", out)
	}
}

// Output is arbitrary text, so it can look like the job shell's records or
// carry a heredoc delimiter's prefix; neither may disturb the job.
func TestOutputShapedLikeTheProtocolIsJustOutput(t *testing.T) {
	executor := newLocalExecutor(t)
	cwdBefore := executor.CWD()

	out := run(t, executor, "echo 'exit 9'\necho 'cwd 4'\necho '/elsewhere'\necho end\necho __YAAIH_0000")

	if want := "exit 9\ncwd 4\n/elsewhere\nend\n__YAAIH_0000\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if executor.CWD() != cwdBefore {
		t.Errorf("CWD = %q, want it kept at %q", executor.CWD(), cwdBefore)
	}
}

// Sourcing a file with a syntax error runs every line in front of the error
// before failing, so unbalanced input has to be refused before any of it runs.
func TestUnbalancedInputIsRefusedWithoutRunningAnyOfIt(t *testing.T) {
	for name, command := range map[string]string{
		"open quote":    "touch ran\necho \"unterminated",
		"open subshell": "touch ran\n( echo inside",
		"open if":       "touch ran\nif true; then echo inside",
	} {
		t.Run(name, func(t *testing.T) {
			executor := newLocalExecutor(t)
			run(t, executor, "mkdir sub && cd sub")
			cwdBefore := executor.CWD()

			result, err := executor.Start(context.Background(), command, 10*time.Second)

			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !strings.Contains(result.Output, syntaxRejectedNote) || result.ExitCode != 2 {
				t.Errorf("exit %d, output %q; want it refused with 2", result.ExitCode, result.Output)
			}
			if _, err := os.Stat(filepath.Join(cwdBefore, "ran")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the line before the syntax error ran (stat err = %v)", err)
			}
			if executor.CWD() != cwdBefore {
				t.Errorf("CWD = %q, want the session kept at %q", executor.CWD(), cwdBefore)
			}
		})
	}
}

// Linux refuses a single argument over 128 KiB, and a file_write carries the
// whole file in its command.
func TestLargeCommandRuns(t *testing.T) {
	executor := newLocalExecutor(t)
	payload := strings.Repeat("x", 1<<20)

	out := run(t, executor, "printf '%s' '"+payload+"' | wc -c")

	if strings.TrimSpace(out) != "1048576" {
		t.Errorf("output = %q, want the whole payload to have arrived", out)
	}
}

func TestStderrIsMergedIntoTheOutput(t *testing.T) {
	executor := newLocalExecutor(t)

	if out := run(t, executor, "echo out; echo err >&2; echo out again"); out != "out\nerr\nout again\n" {
		t.Errorf("output = %q, want both streams in order", out)
	}
}

// A command still running when the call stops waiting keeps running, and the
// session goes on without whatever it changes.
func TestSlowCommandIsBackgroundedAndItsStateIsNotKept(t *testing.T) {
	executor := newLocalExecutor(t)
	cwdBefore := executor.CWD()

	result, err := executor.Start(context.Background(), "mkdir sub && cd sub\necho started\nsleep 0.5\necho done", 200*time.Millisecond)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !result.Running || result.ID == 0 {
		t.Fatalf("result = %+v, want a running job", result)
	}
	if strings.TrimSpace(result.Output) != "started" {
		t.Errorf("output so far = %q, want started", result.Output)
	}
	if result.LogPath == "" {
		t.Error("LogPath is empty, want the job's log")
	}
	if out := run(t, executor, "pwd"); strings.TrimSpace(out) != cwdBefore {
		t.Errorf("pwd while the job runs = %q, want %q", out, cwdBefore)
	}

	waited, err := executor.Wait(context.Background(), result.ID, 10*time.Second)

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited.Running || waited.ExitCode != 0 {
		t.Errorf("after Wait: running %v, exit %d; want finished with 0", waited.Running, waited.ExitCode)
	}
	if strings.TrimSpace(waited.Output) != "done" {
		t.Errorf("Wait output = %q, want only what came after the first read", waited.Output)
	}
	if executor.CWD() != cwdBefore {
		t.Errorf("CWD = %q, want the backgrounded job's cd discarded", executor.CWD())
	}
}

func TestWaitThatTimesOutLeavesTheJobRunning(t *testing.T) {
	executor := newLocalExecutor(t)
	result, _ := executor.Start(context.Background(), "echo first; sleep 1000", 100*time.Millisecond)

	waited, err := executor.Wait(context.Background(), result.ID, 100*time.Millisecond)

	if err != nil || !waited.Running {
		t.Fatalf("Wait = %+v, %v; want the job still running", waited, err)
	}
	if waited.Output != "" {
		t.Errorf("Wait output = %q, want nothing new", waited.Output)
	}
	if waited.Processes == "" {
		t.Error("Processes is empty, want the job's process group described")
	}
}

func TestWaitAndKillRefuseAnUnknownJob(t *testing.T) {
	executor := newLocalExecutor(t)

	if _, err := executor.Wait(context.Background(), 999, time.Millisecond); err == nil {
		t.Error("Wait on an unknown job succeeded")
	}
	if _, err := executor.Kill(context.Background(), 999); err == nil {
		t.Error("Kill on an unknown job succeeded")
	}
}

func TestBackgroundedJobIsReportedOnceItFinishes(t *testing.T) {
	executor := newLocalExecutor(t)
	result, _ := executor.Start(context.Background(), "sleep 0.3; exit 4", 50*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	var finished []JobInfo
	for len(finished) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		finished = executor.TakeFinished()
	}

	if len(finished) != 1 || finished[0].ID != result.ID || finished[0].ExitCode != 4 {
		t.Fatalf("TakeFinished = %+v, want job %d with exit 4", finished, result.ID)
	}
	if again := executor.TakeFinished(); len(again) != 0 {
		t.Errorf("TakeFinished again = %+v, want each end reported once", again)
	}
}

// A job that finishes inside its call has been seen whole; reporting it again
// would only repeat it.
func TestForegroundJobIsNotReportedAsFinished(t *testing.T) {
	executor := newLocalExecutor(t)
	run(t, executor, "echo quick")

	if finished := executor.TakeFinished(); len(finished) != 0 {
		t.Errorf("TakeFinished = %+v, want nothing", finished)
	}
}

func TestRunSeesTheSessionButNeverChangesIt(t *testing.T) {
	executor := newLocalExecutor(t)
	run(t, executor, "export SEEN=yes")
	cwdBefore := executor.CWD()

	result, err := executor.Run(context.Background(), "echo $SEEN\nmkdir sub && cd sub\nexport LEAKED=yes")

	if err != nil || strings.TrimSpace(result.Output) != "yes" {
		t.Fatalf("Run = %q, %v; want it to see the session's export", result.Output, err)
	}
	if executor.CWD() != cwdBefore {
		t.Errorf("CWD = %q, want Run's cd discarded", executor.CWD())
	}
	if out := run(t, executor, "echo ${LEAKED-absent}"); strings.TrimSpace(out) != "absent" {
		t.Errorf("LEAKED = %q, want Run's export discarded", out)
	}
	if jobs := executor.Jobs(); len(jobs) != 2 {
		t.Errorf("Jobs = %+v, want only the two model jobs listed", jobs)
	}
}

func TestRunIsStoppedWhenItsContextEnds(t *testing.T) {
	executor := newLocalExecutor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := executor.Run(ctx, "sleep 1000")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %s, want it stopped at its deadline", elapsed)
	}
}

func TestJobLogHoldsTheOutput(t *testing.T) {
	executor := newLocalExecutor(t)
	result, err := executor.Start(context.Background(), "seq 1 1000", 10*time.Second)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	logged, err := os.ReadFile(filepath.Join(executor.hostLogDir, filepath.Base(result.LogPath)))

	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(logged) != result.Output {
		t.Errorf("log holds %d bytes, output %d; want the same text", len(logged), len(result.Output))
	}
}

// Job ids name the log files, so a later executor on the same workspace must
// not reuse one.
func TestJobIDsContinueAfterExistingLogs(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, filepath.FromSlash(JobLogDir))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "41.log"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	executor, err := newExecutor(context.Background(), localHost{dir: dir}, dir)
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	defer executor.Close()

	result, err := executor.Start(context.Background(), "true", 10*time.Second)

	if err != nil || result.ID != 42 {
		t.Errorf("first job id = %d (err %v), want 42", result.ID, err)
	}
}

func TestRunningJobsAreCapped(t *testing.T) {
	executor := newLocalExecutor(t)
	for range MaxRunningJobs {
		if _, err := executor.Start(context.Background(), "sleep 1000", 0); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	_, err := executor.Start(context.Background(), "echo one too many", 0)

	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("err = %v, want the cap refusing the job", err)
	}
	if running := executor.RunningJobs(); running != MaxRunningJobs {
		t.Errorf("RunningJobs = %d, want %d", running, MaxRunningJobs)
	}
}

func TestCloseIsIdempotentAndStopsFurtherCommands(t *testing.T) {
	executor := newLocalExecutor(t)

	if err := executor.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := executor.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if executor.IsRunning() {
		t.Error("IsRunning = true after Close")
	}
	if _, err := executor.Start(context.Background(), "echo hi", time.Second); err == nil {
		t.Error("Start after Close succeeded")
	}
	if _, err := executor.Run(context.Background(), "echo hi"); err == nil {
		t.Error("Run after Close succeeded")
	}
}
