package executor

import (
	"bufio"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ruvolof/yaaih/internal/logging"
)

// outputGrace is how long a job's output stream is given to drain after the
// job shell reported its end. Output and records travel on separate streams,
// so the last of the output can arrive just after the end record; but a child
// left running in the background can hold the output stream open
// indefinitely, and its later output is its own business.
const outputGrace = 500 * time.Millisecond

// job is one command's process, as its supervisor sees it.
type job struct {
	id      int
	command string
	started time.Time
	// logPath is the log as a path in the container; logFile is the host side.
	logPath string
	logFile *os.File
	// onFinish runs once the job has ended and its result is final.
	onFinish func()

	mu sync.Mutex
	// window holds the newest output; windowBase is the absolute offset of
	// its first byte, and readCursor the absolute offset reads resume from.
	window     []byte
	windowBase int64
	readCursor int64
	lastOutput time.Time
	pgid       int
	pgidKnown  chan struct{}
	proc       *jobProcess

	finished bool
	ended    time.Time
	exitCode int
	state    *shellState
	// backgrounded is set when Start stopped waiting for the job, which then
	// forfeits its state; reported once the model has been told how it ended.
	backgrounded bool
	reported     bool

	done chan struct{}
}

func newJob(id int, command string) *job {
	return &job{
		id:        id,
		command:   command,
		started:   time.Now(),
		exitCode:  -1,
		pgidKnown: make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// supervise follows the job from start to end: it copies the output, reads
// the job shell's records, and closes the process once both are done with.
func (j *job) supervise(proc *jobProcess) {
	defer logging.Recover("executor.job")
	j.mu.Lock()
	j.proc = proc
	j.mu.Unlock()

	outputDone := make(chan struct{})
	go j.copyOutput(proc.output, outputDone)

	report, err := readRecords(proc.meta, j.setPGID)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		log.Debug("job records ended early", "job", j.id, "err", err)
	}
	// The job shell writes nothing after its end record, but whatever is
	// still on its record stream must be read for the output to keep flowing.
	go func() { _, _ = io.Copy(io.Discard, proc.meta) }()

	select {
	case <-outputDone:
	case <-time.After(outputGrace):
	}
	proc.close()
	<-outputDone

	j.mu.Lock()
	j.finished = true
	j.ended = time.Now()
	if report.ended {
		j.exitCode = report.exitCode
		j.state = report.state
	}
	if j.logFile != nil {
		_ = j.logFile.Close()
	}
	onFinish := j.onFinish
	j.mu.Unlock()
	j.setPGID(0)
	close(j.done)
	if onFinish != nil {
		onFinish()
	}
}

func (j *job) copyOutput(output io.Reader, done chan<- struct{}) {
	defer close(done)
	defer logging.Recover("executor.output")
	reader := bufio.NewReaderSize(output, 32<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			j.appendOutput(chunk[:n])
		}
		if err != nil {
			return
		}
	}
}

func (j *job) appendOutput(chunk []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lastOutput = time.Now()
	if j.logFile != nil {
		if _, err := j.logFile.Write(chunk); err != nil {
			log.Warn("job log write failed; the rest of the output stays in memory only", "job", j.id, "err", err)
			_ = j.logFile.Close()
			j.logFile = nil
		}
	}
	j.window = append(j.window, chunk...)
	// Trimming to the cap on every chunk would copy the whole window each
	// time once it is full; letting it reach twice the cap first amortises
	// that to one copy per cap's worth of output.
	if len(j.window) > 2*maxCapturedBytes {
		drop := len(j.window) - maxCapturedBytes
		j.window = append([]byte(nil), j.window[drop:]...)
		j.windowBase += int64(drop)
	}
}

// setPGID records the job shell's process group; zero marks it as never coming.
func (j *job) setPGID(pgid int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	select {
	case <-j.pgidKnown:
		return
	default:
	}
	j.pgid = pgid
	close(j.pgidKnown)
}

// awaitPGID returns the job's process group, waiting up to timeout for the job
// shell to report it. Zero means it is not known.
func (j *job) awaitPGID(timeout time.Duration) int {
	if timeout > 0 {
		select {
		case <-j.pgidKnown:
		case <-time.After(timeout):
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.pgid
}

// abandon ends the job's streams without waiting for the job shell, which
// lets the supervisor finish.
func (j *job) abandon() {
	j.mu.Lock()
	proc := j.proc
	j.mu.Unlock()
	if proc != nil {
		proc.close()
	}
}

// read returns what the job has produced since the last read, and where it is
// now. advance moves the read cursor past it; without it the whole retained
// window comes back, which is what a one-shot run wants.
func (j *job) read(advance bool) JobResult {
	j.mu.Lock()
	defer j.mu.Unlock()
	from := j.windowBase
	if advance && j.readCursor > from {
		from = j.readCursor
	}
	var skipped int64
	if advance {
		skipped = from - j.readCursor
	} else {
		skipped = j.windowBase
	}
	output := string(j.window[from-j.windowBase:])
	if advance {
		j.readCursor = j.windowBase + int64(len(j.window))
	}

	now := time.Now()
	end := now
	if j.finished {
		end = j.ended
	}
	sinceOutput := now.Sub(j.started)
	if !j.lastOutput.IsZero() {
		sinceOutput = now.Sub(j.lastOutput)
	}
	return JobResult{
		ID:          j.id,
		Output:      output,
		Skipped:     skipped,
		Running:     !j.finished,
		ExitCode:    j.exitCode,
		Elapsed:     end.Sub(j.started),
		SinceOutput: sinceOutput,
		LogPath:     j.logPath,
	}
}

func (j *job) info() JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	end := time.Now()
	if j.finished {
		end = j.ended
	}
	return JobInfo{
		ID:       j.id,
		Command:  j.command,
		Running:  !j.finished,
		ExitCode: j.exitCode,
		Elapsed:  end.Sub(j.started),
		LogPath:  j.logPath,
	}
}
