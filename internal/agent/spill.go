// Spill files: the full text of a tool result that was too large to keep in
// the conversation, written where the agent can still go and read it.
package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/ruvolof/yaaih/internal/executor"
)

// spillDirName is where full tool outputs are written, relative to the
// workspace root. Inside the workspace because that is the one directory both
// the harness and the sandbox can see; dot-prefixed to stay out of the way of
// the agent's own files.
const spillDirName = ".harness/tool-output"

// spillRetainedBytes is the total size of spill files kept on disk. Oldest go
// first once the directory exceeds it, so a session that dumps repeatedly does
// not fill the workspace.
const spillRetainedBytes = 64 << 20

// spillToolResult writes the full output of one tool call into the workspace
// and returns a note telling the model how to get at it. An empty return means
// no spill was written, in which case the truncation marker promises nothing.
func (a *Agent) spillToolResult(toolCallID, name, content string) string {
	reach := a.spillAccess()
	if reach == nil {
		return ""
	}

	hostDir := filepath.Join(a.cfg.Runtime.Workspace, filepath.FromSlash(spillDirName))
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		log.Warn("tool output spill unavailable", "err", err, "dir", hostDir)
		return ""
	}

	file := fmt.Sprintf("%04d-%s-%s.txt", atomic.LoadInt64(&a.seq), name, sanitiseForFilename(toolCallID))
	if err := os.WriteFile(filepath.Join(hostDir, file), []byte(content), 0o644); err != nil {
		log.Warn("tool output spill failed", "err", err, "file", file)
		return ""
	}
	pruneSpillDir(hostDir, spillRetainedBytes)

	log.Info("tool output spilled", "tool", name, "file", file, "bytes", len(content))
	return reach(file)
}

// spillAccess returns a function rendering the note for a spill filename, or
// nil when the agent has no tool that could open it. Both tools need the
// sandbox runner; the path has to be expressed in terms of whichever one it
// will be handed to: shell commands take container paths, file_read resolves
// against the workspace root.
func (a *Agent) spillAccess() func(file string) string {
	switch {
	case a.runner != nil && a.cfg.HasCapability("shell"):
		return func(file string) string {
			return "full output: " + path.Join(executor.ContainerWorkspace, spillDirName, file) +
				" — grep, sed or tail it with shell_exec to get the part you need"
		}
	case a.runner != nil && a.cfg.HasCapability("filesystem"):
		return func(file string) string {
			return "full output: " + path.Join(spillDirName, file) +
				" — read it with file_read, a page at a time"
		}
	default:
		return nil
	}
}

// sanitiseForFilename reduces a provider-supplied tool call id to something
// safe to join onto a path.
func sanitiseForFilename(s string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	if len(clean) > 40 {
		clean = clean[:40]
	}
	if clean == "" {
		return "call"
	}
	return clean
}

// pruneSpillDir deletes the oldest spill files until the directory fits in
// budget. Failures are logged and otherwise ignored: losing an old spill is not
// worth failing a turn over.
func pruneSpillDir(dir string, budget int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("spill prune failed to list directory", "err", err, "dir", dir)
		return
	}

	type spill struct {
		name  string
		bytes int64
	}
	files := make([]spill, 0, len(entries))
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || entry.IsDir() {
			continue
		}
		files = append(files, spill{name: entry.Name(), bytes: info.Size()})
		total += info.Size()
	}
	if total <= budget {
		return
	}

	// Names carry a zero-padded sequence prefix, so lexical order is age order.
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, file := range files {
		if total <= budget {
			return
		}
		if err := os.Remove(filepath.Join(dir, file.name)); err != nil {
			log.Warn("spill prune failed to remove file", "err", err, "file", file.name)
			continue
		}
		total -= file.bytes
	}
}
