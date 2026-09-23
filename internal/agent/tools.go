// Tool registry, dispatch, and built-in tool implementations.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ruvolof/aibitat-harness-go/internal/browser"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/executor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/toolout"
)

// ToolFunc is the runtime implementation of a tool. It receives the raw JSON
// args string supplied by the model and returns a human/model-readable string
// result. Errors are returned as ordinary error values; the agent loop wraps
// them so the model can self-correct.
type ToolFunc func(ctx context.Context, args string) (string, error)

// Tool bundles metadata, implementation, and gating policy.
type Tool struct {
	Def        llm.ToolDefinition
	Fn         ToolFunc
	Restricted bool   // requires gatekeeper approval
	Capability string // capability name from agent.md; "" = always available
}

// Registry maps tool names to Tool definitions and exposes the slice form
// passed to the LLM provider. It is safe for concurrent use: the turn loop
// reads it (Definitions/Enabled) while an operator-triggered reload may mutate
// it (Add/RemoveByCapability).
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
}

// grantedBy reports whether the agent's capability set unlocks this tool. An
// untagged tool is always available.
func (t *Tool) grantedBy(granted map[string]struct{}) bool {
	if t.Capability == "" {
		return true
	}
	_, ok := granted[t.Capability]
	return ok
}

func capabilitySet(caps []string) map[string]struct{} {
	granted := make(map[string]struct{}, len(caps))
	for _, capability := range caps {
		granted[capability] = struct{}{}
	}
	return granted
}

// Definitions returns the tool definitions by name, filtered by the agent's
// declared capabilities (no capability tag means always exposed).
//
// The order is sorted rather than whatever the map yields, because it is part
// of every request: providers that cache the tokenised prefix of a prompt see
// a reordered tool list as a different prompt, so a random order re-pays the
// full price of the system prompt and the tool definitions on every iteration
// of every turn.
func (r *Registry) Definitions(caps []string) []llm.ToolDefinition {
	granted := capabilitySet(caps)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []llm.ToolDefinition
	for _, t := range r.tools {
		if !t.grantedBy(granted) {
			continue
		}
		out = append(out, t.Def)
	}
	slices.SortFunc(out, func(a, b llm.ToolDefinition) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// Enabled returns the tool by name only when the agent's capabilities grant
// it, applying the same filter as Definitions. Tool dispatch must go through
// here: a model can name a tool it was never offered, and the built-ins close
// over dependencies that only exist for the capabilities the agent declared.
func (r *Registry) Enabled(name string, caps []string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok || !t.grantedBy(capabilitySet(caps)) {
		return nil, false
	}
	return t, true
}

// Lookup returns the tool by name regardless of the agent's capabilities. It
// answers "is this registered", which is a question about the registry rather
// than about what the model may call — dispatch belongs in Enabled.
func (r *Registry) Lookup(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Add registers (or replaces) a tool by its definition name. Used by external
// packages (e.g. scripttools) to inject tools without reaching into the
// private map. A built-in with the same name is overwritten, so callers should
// avoid colliding names.
func (r *Registry) Add(t *Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Def.Name] = t
}

// RemoveByCapability deletes every tool tagged with the given capability and
// returns how many were removed. Used to clear previously registered script
// tools before a reload without disturbing built-ins (which carry a different
// capability tag, or none).
func (r *Registry) RemoveByCapability(capability string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for name, t := range r.tools {
		if t.Capability == capability {
			delete(r.tools, name)
			n++
		}
	}
	return n
}

// NewRegistry wires the built-in tools against their dependencies.
func NewRegistry(cfg *config.Config, runner executor.Executor, br *browser.Browser) *Registry {
	r := &Registry{tools: map[string]*Tool{}}

	// A capability can be declared in agent.md while the sandbox backing it is
	// absent, so each built-in is registered only once its dependency exists.
	// An unregistered tool is one the model is never offered and cannot call;
	// one closing over a nil dependency would panic on the first call.
	register := func(dependencyReady bool, t *Tool) {
		if !dependencyReady {
			return
		}
		r.tools[t.Def.Name] = t
	}

	register(runner != nil, &Tool{
		Capability: "shell",
		Restricted: true,
		Def: llm.ToolDefinition{
			Name: "shell_exec",
			Description: fmt.Sprintf(
				"Execute a shell command in the persistent bash session inside the sandbox container. CWD and env vars persist across calls. "+
					"Output above %s is cut down before you see it — the first and last part are kept and the full text is written to a file "+
					"named in the result, which you can then grep. Filtering in the command itself (grep, head, tail, wc) gets you a better "+
					"answer than reading the cut-down version of an unfiltered one.",
				toolout.HumanBytes(cfg.Options.MaxToolResultBytes)),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command to execute. May span several lines, including heredocs; stdin is empty, so nothing can prompt for input.",
					},
				},
				"required": []string{"command"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("invalid args: %w", err)
			}
			if strings.TrimSpace(p.Command) == "" {
				return "", errors.New("command is empty")
			}
			out, err := runner.Execute(ctx, p.Command)
			if err != nil {
				if te, ok := err.(*executor.TimeoutError); ok {
					return out + "\n[" + te.Message + "]", nil
				}
				return out, err
			}
			return out, nil
		},
	})

	// Browser tools — only browser_eval is restricted.
	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name: "browser_navigate",
			Description: "Navigate the headless browser to a URL. Returns as soon as the HTML is parsed, " +
				"so content rendered by page scripts may not be there yet — follow with browser_wait_for " +
				"when you need a specific element.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string"},
				},
				"required": []string{"url"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if err := br.Navigate(ctx, p.URL); err != nil {
				return "", err
			}
			return "OK: navigated to " + p.URL, nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_back",
			Description: "Return the current tab to the previous page in its history.",
			Parameters:  emptyObjectSchema(),
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			url, err := br.Back(ctx)
			if err != nil {
				return "", err
			}
			return "OK: back to " + url, nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name: "browser_wait_for",
			Description: "Wait for the element matched by CSS selector to reach a state. " +
				"Use after browser_navigate on script-rendered pages, or to wait for a spinner to go away.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"selector": map[string]any{"type": "string"},
					"state": map[string]any{
						"type":        "string",
						"enum":        []string{"visible", "hidden", "attached", "detached"},
						"description": "Defaults to visible.",
					},
				},
				"required": []string{"selector"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Selector string `json:"selector"`
				State    string `json:"state"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if err := br.WaitFor(ctx, p.Selector, p.State); err != nil {
				return "", err
			}
			state := p.State
			if state == "" {
				state = "visible"
			}
			return "OK: " + p.Selector + " is " + state, nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name: "browser_content",
			Description: "Return the current page as Markdown: title, URL, then the main content with " +
				"navigation and footers dropped and link targets given as absolute URLs.",
			Parameters:  emptyObjectSchema(),
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			return br.Content(ctx)
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_screenshot",
			Description: "Capture a PNG screenshot of the current page. When the model lacks vision support, the result is a Markdown text summary; otherwise it is the absolute path to the saved image.",
			Parameters:  emptyObjectSchema(),
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			path, fallback, err := br.Screenshot(ctx)
			if err != nil {
				return "", err
			}
			if fallback != "" {
				return fmt.Sprintf("Saved screenshot to %s. Text fallback (vision unsupported):\n\n%s", path, fallback), nil
			}
			return path, nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_click",
			Description: "Click an element matched by CSS selector.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"selector": map[string]any{"type": "string"},
				},
				"required": []string{"selector"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Selector string `json:"selector"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if err := br.Click(ctx, p.Selector); err != nil {
				return "", err
			}
			return "OK: clicked " + p.Selector, nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_type",
			Description: "Type text into the element matched by CSS selector.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"selector": map[string]any{"type": "string"},
					"text":     map[string]any{"type": "string"},
				},
				"required": []string{"selector", "text"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Selector string `json:"selector"`
				Text     string `json:"text"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if err := br.Fill(ctx, p.Selector, p.Text); err != nil {
				return "", err
			}
			return "OK: typed into " + p.Selector, nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name: "browser_tabs",
			Description: "List the open browser tabs as index, title, and URL. " +
				"The tab every other browser tool acts on is marked with *. " +
				"Check this after clicking a link that may have opened a new tab.",
			Parameters: emptyObjectSchema(),
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			tabs, err := br.ListTabs(ctx)
			if err != nil {
				return "", err
			}
			return strings.Join(tabs, "\n"), nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_select_tab",
			Description: "Point the other browser tools at a different tab, by its index from browser_tabs.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index": map[string]any{"type": "integer"},
				},
				"required": []string{"index"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			url, err := br.SelectTab(ctx, p.Index)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("OK: now on tab %d — %s", p.Index, url), nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_close_tab",
			Description: "Close a tab by its index from browser_tabs. Closing the active tab moves to another one.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index": map[string]any{"type": "integer"},
				},
				"required": []string{"index"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if err := br.CloseTab(ctx, p.Index); err != nil {
				return "", err
			}
			return fmt.Sprintf("OK: closed tab %d", p.Index), nil
		},
	})

	register(br != nil, &Tool{
		Capability: "browser",
		Restricted: true,
		Def: llm.ToolDefinition{
			Name:        "browser_eval",
			Description: "Evaluate JavaScript in the current page context. Returns the string-coerced result.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"script": map[string]any{"type": "string"},
				},
				"required": []string{"script"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Script string `json:"script"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			return br.Eval(ctx, p.Script)
		},
	})

	// Filesystem tools — read/list are unrestricted, write is restricted. They
	// execute inside the sandbox container through the executor (see
	// containerfs.go), so the workspace bind mount is the boundary the agent
	// cannot reach past — symlinks included.
	defaultReadLines := cfg.Options.MaxFileReadLines
	register(runner != nil, &Tool{
		Capability: "filesystem",
		Def: llm.ToolDefinition{
			Name: "file_read",
			Description: fmt.Sprintf(
				"Read a UTF-8 file from the workspace, one page of lines at a time. Path is relative to the workspace root. "+
					"Returns %d lines by default, preceded by a header naming the range read and the file's total line count; "+
					"pass offset and limit to move the window. Reading a targeted range is much cheaper than paging through a "+
					"whole large file, so narrow it down first when you know what you are looking for.", defaultReadLines),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
					"offset": map[string]any{
						"type":        "integer",
						"description": "First line to read, 1-based. Defaults to 1.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": fmt.Sprintf("How many lines to read. Defaults to %d.", defaultReadLines),
					},
				},
				"required": []string{"path"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if p.Offset <= 0 {
				p.Offset = 1
			}
			if p.Limit <= 0 {
				p.Limit = defaultReadLines
			}
			return containerFileRead(ctx, runner, executor.ContainerWorkspace, p.Path, p.Offset, p.Limit)
		},
	})

	register(runner != nil, &Tool{
		Capability: "filesystem",
		Restricted: true,
		Def: llm.ToolDefinition{
			Name:        "file_write",
			Description: "Write a UTF-8 file in the workspace. Creates parent directories. Path is relative to the workspace root.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"path", "content"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			return containerFileWrite(ctx, runner, executor.ContainerWorkspace, p.Path, p.Content)
		},
	})

	register(runner != nil, &Tool{
		Capability: "filesystem",
		Def: llm.ToolDefinition{
			Name:        "file_list",
			Description: "List entries in a workspace directory (non-recursive).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Relative dir; \".\" for workspace root."},
				},
				"required": []string{"path"},
			},
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			var p struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			return containerFileList(ctx, runner, executor.ContainerWorkspace, p.Path)
		},
	})

	return r
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
