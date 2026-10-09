// Built-in filesystem tools, executed inside the sandbox container.
//
// file_read, file_write, and file_list are dispatched through the executor so
// that every path — symlinks included — is resolved by the kernel inside the
// container's mount namespace, where the workspace bind mount is the boundary.
// A host-side path check cannot provide that guarantee: an agent with a shell
// can create a symlink inside the workspace that points anywhere on the host,
// and no lexical inspection of the path sees through it.
//
// Each operation is one shell command that prints its payload followed by a
// status line opening with containerFSMarker. Paths and file content travel
// base64-encoded, so no shell-quoting hazard exists.
package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ruvolof/yaaih/internal/executor"
)

// containerFSMarker opens the status line a filesystem command prints after
// its payload. The parser matches it only when preceded by a newline the
// command itself printed and takes the last occurrence, so payload bytes that
// happen to contain the text cannot displace the real status.
const containerFSMarker = "__HARNESS_FS__"

// containerFSTimeout bounds one filesystem command. They read or write one
// file, so one that runs this long is stuck — on a hung mount, say — and is
// killed rather than left in the background.
const containerFSTimeout = 30 * time.Second

// runContainerFS runs one filesystem command under containerFSTimeout.
func runContainerFS(ctx context.Context, runner executor.Runner, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, containerFSTimeout)
	defer cancel()
	result, err := runner.Run(ctx, cmd)
	return result.Output, err
}

// resolveContainerPath joins rel onto the container workspace root, rejecting
// absolute paths and lexical escapes via "..". This enforces the tool contract
// (paths are workspace-relative); containment against everything the check
// cannot see — symlinks above all — comes from the command executing inside
// the container, where the bind mount is the boundary.
func resolveContainerPath(root, rel string) (string, error) {
	// Control characters are rejected because the path is recovered in the
	// shell via $(…), which strips trailing newlines — "name\n" would silently
	// address "name".
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("path contains control characters: %q", rel)
		}
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the workspace: %q", rel)
	}
	full := path.Join(root, rel)
	if full != root && !strings.HasPrefix(full, root+"/") {
		return "", fmt.Errorf("path escapes workspace: %q", rel)
	}
	return full, nil
}

func b64Arg(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// fileReadCommand prints lines [offset, offset+limit) of the file, then the
// status line with the file's total line count. `grep -c ''` counts a final
// line with no trailing newline, matching how the harness numbers lines.
func fileReadCommand(containerPath string, offset, limit int) string {
	return fmt.Sprintf(
		`p=$(printf '%%s' '%[1]s' | base64 -d); `+
			`if [ -d "$p" ]; then printf '\n%[2]s err is a directory'; `+
			`elif [ ! -e "$p" ]; then printf '\n%[2]s err no such file'; `+
			`elif [ ! -r "$p" ]; then printf '\n%[2]s err permission denied'; `+
			`else sed -n '%[3]d,%[4]dp' "$p" 2>/dev/null; printf '\n%[2]s ok %%s' "$(grep -c '' "$p")"; fi`,
		b64Arg(containerPath), containerFSMarker, offset, offset+limit-1)
}

// fileWriteCommand creates the parent directories and writes the decoded
// content. stderr is left on the stream deliberately: on failure it lands in
// the payload, which containerFileWrite folds into the error the model sees.
func fileWriteCommand(containerPath, content string) string {
	return fmt.Sprintf(
		`p=$(printf '%%s' '%[1]s' | base64 -d); `+
			`if [ -d "$p" ]; then printf '\n%[2]s err is a directory'; `+
			`elif mkdir -p "$(dirname "$p")" && printf '%%s' '%[3]s' | base64 -d > "$p"; then printf '\n%[2]s ok'; `+
			`else printf '\n%[2]s err write failed'; fi`,
		b64Arg(containerPath), containerFSMarker, b64Arg(content))
}

// fileListCommand lists one directory. `-A` includes dotfiles and `-p` marks
// directories with a trailing slash, which containerFileList turns back into
// the `d  name` / `f  name` lines the tool has always produced; LC_ALL=C keeps
// the ordering a plain byte sort.
func fileListCommand(containerPath string) string {
	return fmt.Sprintf(
		`p=$(printf '%%s' '%[1]s' | base64 -d); `+
			`if [ ! -e "$p" ]; then printf '\n%[2]s err no such directory'; `+
			`elif [ ! -d "$p" ]; then printf '\n%[2]s err not a directory'; `+
			`else LC_ALL=C ls -1Ap "$p" 2>/dev/null; printf '\n%[2]s ok'; fi`,
		b64Arg(containerPath), containerFSMarker)
}

// splitContainerFSResult separates a command's payload from its trailing
// status line. ok is false when the marker never arrived — the command was
// interrupted or its output mangled in transit. The status line ends at the
// first newline after the marker: anything beyond it is the executor's own
// annotation (e.g. its capture-cap note), not part of the status.
func splitContainerFSResult(raw string) (payload, status, detail string, ok bool) {
	idx := strings.LastIndex(raw, "\n"+containerFSMarker+" ")
	if idx < 0 {
		return "", "", "", false
	}
	payload = raw[:idx]
	meta := raw[idx+1+len(containerFSMarker)+1:]
	if nl := strings.IndexByte(meta, '\n'); nl >= 0 {
		meta = meta[:nl]
	}
	status, detail, _ = strings.Cut(strings.TrimSpace(meta), " ")
	return payload, status, detail, true
}

// containerFileRead reads lines [offset, offset+limit) of a workspace file
// through the sandbox and prefixes the page with its describeWindow header.
func containerFileRead(ctx context.Context, runner executor.Runner, root, rel string, offset, limit int) (string, error) {
	containerPath, err := resolveContainerPath(root, rel)
	if err != nil {
		return "", err
	}
	raw, err := runContainerFS(ctx, runner, fileReadCommand(containerPath, offset, limit))
	if err != nil {
		return "", err
	}
	window, status, detail, ok := splitContainerFSResult(raw)
	if !ok {
		return "", fmt.Errorf("read %s: sandbox returned no status: %s", rel, clip(strings.TrimSpace(raw), 200))
	}
	if status != "ok" {
		return "", fmt.Errorf("read %s: %s", rel, detail)
	}
	total, err := strconv.Atoi(detail)
	if err != nil {
		return "", fmt.Errorf("read %s: bad line count %q from sandbox", rel, detail)
	}
	return describeWindow(rel, offset, limit, total) + window, nil
}

// containerFileWrite writes content to a workspace file through the sandbox,
// creating parent directories.
func containerFileWrite(ctx context.Context, runner executor.Runner, root, rel, content string) (string, error) {
	containerPath, err := resolveContainerPath(root, rel)
	if err != nil {
		return "", err
	}
	raw, err := runContainerFS(ctx, runner, fileWriteCommand(containerPath, content))
	if err != nil {
		return "", err
	}
	payload, status, detail, ok := splitContainerFSResult(raw)
	if !ok {
		return "", fmt.Errorf("write %s: sandbox returned no status: %s", rel, clip(strings.TrimSpace(raw), 200))
	}
	if status != "ok" {
		if diag := strings.TrimSpace(payload); diag != "" {
			detail += ": " + clip(diag, 200)
		}
		return "", fmt.Errorf("write %s: %s", rel, detail)
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), rel), nil
}

// containerFileList lists a workspace directory through the sandbox.
func containerFileList(ctx context.Context, runner executor.Runner, root, rel string) (string, error) {
	containerPath, err := resolveContainerPath(root, rel)
	if err != nil {
		return "", err
	}
	raw, err := runContainerFS(ctx, runner, fileListCommand(containerPath))
	if err != nil {
		return "", err
	}
	payload, status, detail, ok := splitContainerFSResult(raw)
	if !ok {
		return "", fmt.Errorf("list %s: sandbox returned no status: %s", rel, clip(strings.TrimSpace(raw), 200))
	}
	if status != "ok" {
		return "", fmt.Errorf("list %s: %s", rel, detail)
	}
	var entries strings.Builder
	for _, name := range strings.Split(payload, "\n") {
		if name == "" {
			continue
		}
		if strings.HasSuffix(name, "/") {
			fmt.Fprintf(&entries, "d  %s\n", strings.TrimSuffix(name, "/"))
		} else {
			fmt.Fprintf(&entries, "f  %s\n", name)
		}
	}
	return entries.String(), nil
}

// describeWindow is the header above a page of file content: which lines these
// are, how many there are in total, and how to ask for the next page. Without
// it a page reads like a whole file that happens to end abruptly.
func describeWindow(displayPath string, offset, limit, total int) string {
	if total == 0 {
		return fmt.Sprintf("[%s: empty file]\n", displayPath)
	}
	if offset > total {
		return fmt.Sprintf("[%s: offset %d is past the end; the file has %d lines]\n", displayPath, offset, total)
	}
	last := min(offset+limit-1, total)
	if last < total {
		return fmt.Sprintf("[%s: lines %d-%d of %d — call again with offset %d for the next page]\n",
			displayPath, offset, last, total, last+1)
	}
	return fmt.Sprintf("[%s: lines %d-%d of %d]\n", displayPath, offset, last, total)
}

// clip bounds a diagnostic string quoted inside an error message.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
