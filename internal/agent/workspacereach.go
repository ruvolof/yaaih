// How the agent reaches a file the harness wrote into the workspace, and how
// to name that file so the reaching tool can open it.
package agent

import (
	"path"

	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/executor"
)

// workspaceReach is the tool the agent would open a workspace file with. The
// harness sees the workspace as a host directory, but the agent never does:
// shell commands run in the sandbox, where it is mounted at
// executor.ContainerWorkspace, and the file tools resolve paths against the
// workspace root. A path handed to the model has to be in one of those forms.
type workspaceReach int

const (
	reachNone workspaceReach = iota
	reachShell
	reachFilesystem
)

// workspaceReachFor picks the tool the agent has for opening workspace files,
// preferring the shell. Both need the sandbox runner.
func workspaceReachFor(cfg *config.Config, runner executor.Executor) workspaceReach {
	switch {
	case runner != nil && cfg.HasCapability("shell"):
		return reachShell
	case runner != nil && cfg.HasCapability("filesystem"):
		return reachFilesystem
	default:
		return reachNone
	}
}

// path renders a slash-separated path relative to the workspace root in the
// form the reaching tool takes. With no tool to reach it, the relative path is
// still the one the operator would recognise.
func (reach workspaceReach) path(relPath string) string {
	if reach == reachShell {
		return path.Join(executor.ContainerWorkspace, relPath)
	}
	return relPath
}
