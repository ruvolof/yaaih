package executor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// shellHost starts the processes jobs run in. Splitting it out keeps the job
// protocol — the part with the invariants worth getting right — independent of
// how the process is attached: Docker exec in production, a host bash in tests.
type shellHost interface {
	// startJob runs jobBootstrap with script on its stdin. The process must
	// lead its own process group.
	startJob(ctx context.Context, script string) (*jobProcess, error)
	// runHelper runs a short bash script to completion and returns its stdout
	// and stderr.
	runHelper(ctx context.Context, script string) (string, error)
}

// jobProcess is one started job: its output — the command's stdout and stderr,
// which the job shell merges — and, on a stream of its own, the records the
// job shell writes about itself.
type jobProcess struct {
	output io.Reader
	meta   io.Reader
	close  func()
}

// jobBootstrap is the command every job process runs. The job script arrives
// on stdin rather than as an argument because Linux caps one argument at
// 128 KiB, and a file_write carries the whole file in its command. Reading it
// all before running any of it also leaves the command with an exhausted
// stdin, so nothing it runs can wait for input.
const jobBootstrap = `eval "$(cat)"`

// shellState is a snapshot of the session's shell, as the job shell reported it.
type shellState struct {
	cwd string
	// declarations recreates variables, functions, and aliases.
	declarations string
	// shopts is `shopt -p` output; setopts is $SHELLOPTS.
	shopts  string
	setopts string
}

// restorableSetOptions are the `set -o` options carried from job to job. The
// rest either do nothing without a terminal or, like noexec, would stop every
// later command from running at all.
var restorableSetOptions = []string{
	"allexport", "braceexpand", "errexit", "errtrace", "functrace", "hashall", "keyword",
	"noclobber", "noglob", "nounset", "physical", "pipefail", "posix",
	// Last, so that restoring the others is neither echoed nor traced.
	"verbose", "xtrace",
}

// setOptionsPrefix turns the snapshot's `set -o` options back into commands,
// joined onto one line that is put in front of the command's own first line.
// Restoring them there, rather than in the job script, means they are in force
// for the command alone: errexit or nounset would otherwise end the job script
// early, and xtrace would trace it. On the same line, the command's line
// numbers in its error messages stay its own.
//
// The shopt settings cannot ride along: bash parses a whole line before running
// any of it, so expand_aliases set on the command's first line would come too
// late for the aliases on it. The job script restores those itself.
func (s *shellState) setOptionsPrefix() string {
	var prefix strings.Builder
	enabled := map[string]bool{}
	for _, option := range strings.Split(s.setopts, ":") {
		enabled[option] = true
	}
	for _, option := range restorableSetOptions {
		flag := "+o"
		if enabled[option] {
			flag = "-o"
		}
		fmt.Fprintf(&prefix, "set %s %s; ", flag, option)
	}
	return prefix.String()
}

// syntaxRejectedNote follows bash's own parse error when a command is refused,
// so the model does not read the error as coming from a command that ran.
const syntaxRejectedNote = "[not executed: bash rejected the syntax of this command, so none of it ran]"

// saveStateFunction reports the shell's state on the record stream and ends
// the record stream with "end". The trap captures $?, $SHELLOPTS, and the
// shopt settings before calling it, with tracing silenced, so neither the
// capture nor a session's own `set -x` shows up in the output.
//
// Variables come one `declare -p` at a time so readonly ones and bash's own
// dynamic ones can be skipped; restoring those either fails or lies. Nothing
// the dump depends on is made local before it runs — a local IFS or LC_ALL
// would be dumped in place of the session's own. Lengths are byte counts under
// LC_ALL=C, so values may hold anything, newlines included.
const saveStateFunction = `__yaaih_save() {
	[[ -n ${__yaaih_saved-} ]] && return
	__yaaih_saved=1
	local __yaaih_rc=$1 __yaaih_name __yaaih_names __yaaih_state __yaaih_cwd=$PWD
	set +eu
	__yaaih_state=$(
		mapfile -t __yaaih_names < <(compgen -v)
		for __yaaih_name in "${__yaaih_names[@]}"; do
			case $__yaaih_name in
			__yaaih_* | BASH* | PWD | _ | SHLVL | PPID | UID | EUID | GROUPS | FUNCNAME | DIRSTACK | PIPESTATUS | \
				RANDOM | SRANDOM | SECONDS | LINENO | HISTCMD | EPOCHSECONDS | EPOCHREALTIME | COMP_WORDBREAKS | \
				SHELLOPTS | BASHOPTS) continue ;;
			esac
			[[ ${!__yaaih_name@a} == *r* ]] && continue
			declare -p "$__yaaih_name"
		done
		mapfile -t __yaaih_names < <(compgen -A function)
		for __yaaih_name in "${__yaaih_names[@]}"; do
			[[ $__yaaih_name == __yaaih_* ]] || declare -f "$__yaaih_name"
		done
		alias -p
	)
	local LC_ALL=C
	builtin printf 'exit %d\ncwd %d\n%sstate %d\n%sshopts %d\n%ssetopts %d\n%send\n' \
		"$__yaaih_rc" "${#__yaaih_cwd}" "$__yaaih_cwd" "${#__yaaih_state}" "$__yaaih_state" \
		"${#__yaaih_shopt}" "$__yaaih_shopt" "${#__yaaih_setopt}" "$__yaaih_setopt" >&"$__yaaih_meta"
}
`

// captureBeforeSave records what saveStateFunction reports, inside a group
// whose stderr — where an xtrace of it would go — is discarded.
const captureBeforeSave = `{ __yaaih_rc=$?; __yaaih_setopt=$SHELLOPTS; __yaaih_shopt=$(shopt -p); set +xv; } 2>/dev/null`

// newDelimiter returns a heredoc delimiter no command will contain.
func newDelimiter() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return "__YAAIH_" + hex.EncodeToString(nonce), nil
}

// jobScript is what a job shell runs. It moves the shell's stderr onto its
// stdout, so the command's two streams arrive merged in order, and keeps the
// original stderr as the record stream. The command never sees that stream:
// it is closed for the source, so nothing the command starts can hold it open
// or write into it.
//
// The command is checked with `bash -n` before anything runs, because sourcing
// stops at a syntax error only after executing every line in front of it.
// extglob is enabled for the check because the session may have turned it on,
// and a pattern like !(x) is a syntax error to a bash without it. The command
// is sourced, not run as a child, so cd, export, and function definitions land
// in the shell whose state is reported.
//
// The state is saved both after the source returns and from an EXIT trap: the
// first covers a command that sets an EXIT trap of its own, the second one
// that calls exit or trips errexit.
func jobScript(command string, snapshot *shellState, delimiter string) string {
	if !strings.HasSuffix(command, "\n") {
		command += "\n"
	}
	optionsPrefix := ""
	if snapshot != nil {
		optionsPrefix = snapshot.setOptionsPrefix()
	}
	var script strings.Builder
	script.WriteString("exec {__yaaih_meta}>&2 2>&1\n")
	script.WriteString("builtin printf 'pid %d\\n' \"$$\" >&\"$__yaaih_meta\"\n")
	if snapshot != nil {
		script.WriteString("{\nunset $(compgen -e)\n")
		script.WriteString(snapshot.declarations)
		script.WriteString("\n} 2>/dev/null\n")
		fmt.Fprintf(&script, "builtin cd -- %s 2>/dev/null || { builtin cd -- %s; builtin printf '%%s\\n' %s; }\n",
			shellQuote(snapshot.cwd), shellQuote(ContainerWorkspace),
			shellQuote(fmt.Sprintf("[the working directory %s no longer exists; this command ran in %s]", snapshot.cwd, ContainerWorkspace)))
	}
	script.WriteString(saveStateFunction)
	fmt.Fprintf(&script, "trap %s EXIT\n", shellQuote(captureBeforeSave+`; __yaaih_save "$__yaaih_rc"`))
	fmt.Fprintf(&script, "IFS= builtin read -r -d '' __yaaih_command <<'%s'\n%s%s\n", delimiter, command, delimiter)
	fmt.Fprintf(&script, "__yaaih_options=%s\n", shellQuote(optionsPrefix))
	if snapshot != nil {
		fmt.Fprintf(&script, "{\n%s\n} 2>/dev/null\n", snapshot.shopts)
	}
	script.WriteString("if \"$BASH\" -O extglob -n <<<\"$__yaaih_command\"; then\n")
	script.WriteString(". <(builtin printf '%s%s' \"$__yaaih_options\" \"$__yaaih_command\") </dev/null {__yaaih_meta}>&-\n")
	script.WriteString(captureBeforeSave + "\n")
	script.WriteString("__yaaih_save \"$__yaaih_rc\"\n")
	script.WriteString("builtin exit \"$__yaaih_rc\"\n")
	script.WriteString("else\n")
	fmt.Fprintf(&script, "builtin printf '%%s\\n' %s\n", shellQuote(syntaxRejectedNote))
	script.WriteString("builtin exit 2\n")
	script.WriteString("fi\n")
	return script.String()
}

// shellQuote single-quotes s for bash.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// jobReport is what the job shell reported about itself.
type jobReport struct {
	ended    bool
	exitCode int
	state    *shellState
}

// readRecords reads the record stream until the job shell's end record or the
// stream's end. pid is called once the job shell has reported its process
// group. A stream that ends without an end record means the shell was killed,
// or replaced itself with exec, before it could report.
func readRecords(meta io.Reader, pid func(int)) (jobReport, error) {
	reader := bufio.NewReader(meta)
	var report jobReport
	state := &shellState{}
	readPayload := func(length string) (string, error) {
		size, err := strconv.Atoi(length)
		if err != nil || size < 0 {
			return "", fmt.Errorf("bad record length %q", length)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return "", err
		}
		return string(payload), nil
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return report, err
		}
		key, value, _ := strings.Cut(strings.TrimRight(line, "\n"), " ")
		switch key {
		case "pid":
			pgid, convErr := strconv.Atoi(value)
			// Pids 0 and 1 stand for the caller's own group and for every
			// process there is; neither can be a job's group.
			if convErr != nil || pgid <= 1 {
				return report, fmt.Errorf("bad pid record %q", value)
			}
			pid(pgid)
		case "exit":
			if report.exitCode, err = strconv.Atoi(value); err != nil {
				return report, fmt.Errorf("bad exit record %q", value)
			}
		case "cwd":
			if state.cwd, err = readPayload(value); err != nil {
				return report, err
			}
		case "state":
			if state.declarations, err = readPayload(value); err != nil {
				return report, err
			}
		case "shopts":
			if state.shopts, err = readPayload(value); err != nil {
				return report, err
			}
		case "setopts":
			if state.setopts, err = readPayload(value); err != nil {
				return report, err
			}
		case "end":
			report.ended = true
			report.state = state
			return report, nil
		default:
			return report, fmt.Errorf("unknown record %q", truncate(line, 80))
		}
	}
}

// killScript sends SIGKILL to every process in the group and prints "stopped"
// once none is left. A group that no longer exists counts as stopped.
func killScript(pgid int) string {
	return fmt.Sprintf("if kill -KILL -- -%[1]d 2>/dev/null || ! kill -0 -- -%[1]d 2>/dev/null; then echo stopped; fi", pgid)
}

// processListScript lists every process with the fields describeProcessGroup
// reads. The filtering happens on the host: ps's own group selection differs
// between implementations.
const processListScript = "ps -e -o pid=,pgid=,stat=,pcpu=,comm="

// describeProcessGroup summarises the processes of group pgid from
// processListScript's output, leaving out the job shell itself, which only
// waits on the rest: "2 processes: npm (sleeping), node (running, 97% cpu)".
func describeProcessGroup(listing string, pgid int) string {
	var described []string
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		group, groupErr := strconv.Atoi(fields[1])
		if pidErr != nil || groupErr != nil || group != pgid || pid == pgid {
			continue
		}
		detail := processState(fields[2])
		if cpu, err := strconv.ParseFloat(fields[3], 64); err == nil && cpu >= 1 {
			detail += fmt.Sprintf(", %.0f%% cpu", cpu)
		}
		described = append(described, fmt.Sprintf("%s (%s)", strings.Join(fields[4:], " "), detail))
	}
	switch len(described) {
	case 0:
		return "no processes besides the job shell"
	case 1:
		return "1 process: " + described[0]
	default:
		return fmt.Sprintf("%d processes: %s", len(described), strings.Join(described, ", "))
	}
}

func processState(stat string) string {
	if stat == "" {
		return "unknown"
	}
	switch stat[0] {
	case 'R':
		return "running"
	case 'S', 'I':
		return "sleeping"
	case 'D':
		return "waiting on I/O"
	case 'T', 't':
		return "stopped"
	case 'Z':
		return "zombie"
	default:
		return stat
	}
}
