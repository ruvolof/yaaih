// Package scripttools loads user-supplied script tools from a `tools/`
// directory next to the agent.md. Each tool is described by a JSON manifest
// alongside its executable. At startup, every manifest registers a new
// restricted tool in the agent registry; when the model invokes it, the script
// runs inside the runtime container (not on the host) via the shared executor.
//
// Calling convention: the model's JSON arguments are written to a temp file
// inside the container and the path is passed to the script as $1. The script
// reads its arguments from that file. Its stdout becomes the tool result; a
// non-zero exit becomes a tool error the model can see and self-correct from.
package scripttools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/executor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var log = logging.For("scripttools")

// Capability is the agent.md capability that must be granted for script tools
// to be exposed to the model. Kept explicit (opt-in) rather than implied by
// the presence of a tools/ directory.
const Capability = "scripts"

// Container paths where script tools are found at runtime:
//   - ContainerToolsMount is the read-only bind mount of the persistent tool
//     folder (promoted tools).
//   - ContainerWorkspaceTools is the workspace's own tools/ directory, where an
//     agent drafts tools (only consulted when enable_workspace_tools is set).
const (
	ContainerToolsMount     = "/tools"
	ContainerWorkspaceTools = "/workspace/tools"
)

// exitMarker delimits script stdout from the captured exit code in the wrapped
// shell command. Chosen to be unlikely to appear in ordinary output.
const exitMarker = "__SCRIPT_EXIT__:"

// defaultTimeoutSeconds applies when a manifest omits timeout_seconds.
const defaultTimeoutSeconds = 30

// nameRE matches OpenAI tool-name constraints.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Manifest is one tool's on-disk description.
type Manifest struct {
	Name           string                 `json:"name"`
	Description    string                 `json:"description"`
	Executable     string                 `json:"executable"`
	Parameters     map[string]interface{} `json:"parameters"`
	Restricted     *bool                  `json:"restricted"` // pointer so default-true works
	TimeoutSeconds int                    `json:"timeout_seconds"`
}

// isRestricted reports the effective restricted flag (default true).
func (m Manifest) isRestricted() bool {
	return m.Restricted == nil || *m.Restricted
}

// timeout returns the effective per-invocation timeout.
func (m Manifest) timeout() time.Duration {
	if m.TimeoutSeconds <= 0 {
		return defaultTimeoutSeconds * time.Second
	}
	return time.Duration(m.TimeoutSeconds) * time.Second
}

// Load returns the validated manifests found under dir. A missing dir returns
// (nil, nil). Per-file errors (bad JSON, failed validation, missing
// executable) are logged and skipped so one bad tool doesn't sink the batch.
func Load(dir string) ([]Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tools dir %s: %w", dir, err)
	}

	var out []Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		abs := filepath.Join(dir, e.Name())
		m, ok := loadOne(dir, abs)
		if !ok {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// loadOne parses and validates a single manifest. Returns ok=false (after
// logging) on any error.
func loadOne(dir, abs string) (Manifest, bool) {
	raw, err := os.ReadFile(abs)
	if err != nil {
		log.Warn("manifest read failed, skipping", "path", abs, "err", err.Error())
		return Manifest{}, false
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Warn("manifest parse failed, skipping", "path", abs, "err", err.Error())
		return Manifest{}, false
	}
	if !nameRE.MatchString(m.Name) {
		log.Warn("manifest rejected: invalid name", "path", abs, "name", m.Name)
		return Manifest{}, false
	}
	if strings.TrimSpace(m.Executable) == "" {
		log.Warn("manifest rejected: missing executable", "path", abs, "name", m.Name)
		return Manifest{}, false
	}
	if m.Parameters == nil {
		log.Warn("manifest rejected: missing parameters schema", "path", abs, "name", m.Name)
		return Manifest{}, false
	}
	// The executable must exist next to the manifest as a regular file. The
	// executable bit is enforced inside the container at run time (host may be
	// Windows, where the bit is meaningless).
	execPath := filepath.Join(dir, m.Executable)
	info, err := os.Stat(execPath)
	if err != nil {
		log.Warn("manifest rejected: executable not found", "path", abs, "executable", execPath, "err", err.Error())
		return Manifest{}, false
	}
	if !info.Mode().IsRegular() {
		log.Warn("manifest rejected: executable is not a regular file", "path", abs, "executable", execPath)
		return Manifest{}, false
	}
	return m, true
}

// Apply clears the previously registered script tools and registers the
// current set from the live tool locations. Returns the number of tools
// registered. Built-in tools are left untouched. Both the initial startup load
// and the operator's /reload use this, so new, changed, and removed tools are
// reflected consistently — without copying anything.
//
// Tools are loaded from the persistent tool folder (toolsDir, mounted
// read-only at /tools). When enableWorkspaceTools is true, tools the agent
// drafted into <workspace>/tools are also loaded; a workspace draft shadows a
// promoted tool of the same name so the agent can iterate before the operator
// promotes it.
func Apply(toolsDir, workspaceDir string, enableWorkspaceTools bool, runner executor.Executor, registry *agent.Registry) (int, error) {
	registry.RemoveByCapability(Capability)

	total, err := loadAndRegister(toolsDir, ContainerToolsMount, runner, registry)
	if err != nil {
		return total, err
	}
	if enableWorkspaceTools {
		n, err := loadAndRegister(filepath.Join(workspaceDir, "tools"), ContainerWorkspaceTools, runner, registry)
		if err != nil {
			return total, err
		}
		total += n
	}
	log.Info("script tools applied", "registered", total, "workspace_tools", enableWorkspaceTools)
	return total, nil
}

// loadAndRegister loads the manifests in hostDir and registers them to run from
// containerDir inside the sandbox. A missing hostDir contributes nothing.
func loadAndRegister(hostDir, containerDir string, runner executor.Executor, registry *agent.Registry) (int, error) {
	manifests, err := Load(hostDir)
	if err != nil {
		return 0, fmt.Errorf("load manifests from %s: %w", hostDir, err)
	}
	Register(manifests, containerDir, runner, registry)
	return len(manifests), nil
}

// Register builds a Tool for each manifest and adds it to the registry. The
// Tool's Fn shells out to the script at containerDir/<executable> inside the
// container via runner.
func Register(manifests []Manifest, containerDir string, runner executor.Executor, registry *agent.Registry) {
	for _, m := range manifests {
		registry.Add(&agent.Tool{
			Def: llm.ToolDefinition{
				Name:        m.Name,
				Description: m.Description,
				Parameters:  m.Parameters,
			},
			Fn:         buildScriptFn(m, containerDir, runner),
			Restricted: m.isRestricted(),
			Capability: Capability,
		})
		log.Info("script tool registered",
			"name", m.Name,
			"executable", m.Executable,
			"container_dir", containerDir,
			"restricted", m.isRestricted(),
			"timeout_sec", int(m.timeout().Seconds()),
		)
	}
}

// buildScriptFn returns the ToolFunc that runs one script invocation.
// containerDir is where the executable lives inside the sandbox. The read-only
// /tools mount cannot be chmod'd, so its scripts must already be executable;
// scripts under the writable workspace are chmod'd at run time.
func buildScriptFn(m Manifest, containerDir string, runner executor.Executor) agent.ToolFunc {
	scriptPath := path.Join(containerDir, filepath.ToSlash(m.Executable))
	writable := containerDir != ContainerToolsMount
	return func(ctx context.Context, args string) (string, error) {
		if runner == nil {
			return "", fmt.Errorf("script tool %q requires the runtime sandbox, which is not running", m.Name)
		}
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}

		// Bound the call to the manifest timeout. The executor additionally
		// caps every command at the configured command_timeout, so the
		// effective limit is whichever is smaller — no extra plumbing needed.
		runCtx := ctx
		if d := m.timeout(); d > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}

		out, err := runner.Execute(runCtx, buildCommand(scriptPath, args, writable))
		if err != nil {
			// Timeouts and executor failures surface as-is; the agent loop
			// renders timeouts specially.
			return out, err
		}
		return parseResult(m.Name, out)
	}
}

// buildCommand wraps the script invocation so that (a) the JSON args reach the
// script via a temp file passed as $1 with no shell-quoting hazard (base64),
// (b) the executable bit is ensured when the script lives on a writable mount
// (workspace) — the read-only /tools mount cannot be chmod'd, so promoted tools
// must already be executable — and (c) the exit code is captured after a marker
// for parseResult.
func buildCommand(scriptPath, args string, writable bool) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(args))
	chmod := ""
	if writable {
		chmod = fmt.Sprintf("chmod +x '%s' 2>/dev/null; ", scriptPath)
	}
	// All substituted values are either a fixed path or base64 (safe charset),
	// so single-quoting is sufficient.
	return fmt.Sprintf(
		"f=$(mktemp); printf '%%s' '%s' | base64 -d > \"$f\"; "+
			"%s'%s' \"$f\"; rc=$?; rm -f \"$f\"; printf '\\n%s%%s\\n' \"$rc\"",
		b64, chmod, scriptPath, exitMarker,
	)
}

// parseResult splits the script's stdout from the trailing exit marker. On a
// non-zero exit it returns an error whose message includes the last line of
// stdout, so the model gets an actionable signal.
func parseResult(name, raw string) (string, error) {
	idx := strings.LastIndex(raw, exitMarker)
	if idx < 0 {
		// Marker missing — the script or shell likely died unexpectedly.
		// Return whatever we captured rather than inventing an exit code.
		return strings.TrimRight(raw, "\r\n"), nil
	}
	stdout := strings.TrimRight(raw[:idx], "\r\n")
	codeStr := strings.TrimSpace(raw[idx+len(exitMarker):])
	code, convErr := strconv.Atoi(codeStr)
	if convErr != nil {
		log.Warn("could not parse script exit code", "tool", name, "raw_code", codeStr)
		return stdout, nil
	}
	if code != 0 {
		return stdout, fmt.Errorf("script exited %d: %s", code, lastLine(stdout))
	}
	return stdout, nil
}

// lastLine returns the final non-empty line of s, for the error summary.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
