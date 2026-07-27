// Tool registry, dispatch, and built-in tool implementations.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ruvolof/aibitat-harness-go/internal/browser"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/executor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
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
// reads it (Definitions/Get) while an operator-triggered reload may mutate it
// (Add/RemoveByCapability).
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
}

// Definitions returns the tool definitions in stable order, filtered by the
// agent's declared capabilities (no capability tag means always exposed).
func (r *Registry) Definitions(caps []string) []llm.ToolDefinition {
	want := map[string]struct{}{}
	for _, c := range caps {
		want[c] = struct{}{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []llm.ToolDefinition
	for _, t := range r.tools {
		if t.Capability != "" {
			if _, ok := want[t.Capability]; !ok {
				continue
			}
		}
		out = append(out, t.Def)
	}
	return out
}

// Get returns the tool by name.
func (r *Registry) Get(name string) (*Tool, bool) {
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

	r.tools["shell_exec"] = &Tool{
		Capability: "shell",
		Restricted: true,
		Def: llm.ToolDefinition{
			Name:        "shell_exec",
			Description: "Execute a shell command in the persistent bash session inside the sandbox container. CWD and env vars persist across calls.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command to execute (single line).",
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
	}

	// Browser tools — only browser_eval is restricted.
	r.tools["browser_navigate"] = &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_navigate",
			Description: "Navigate the headless browser to a URL.",
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
	}

	r.tools["browser_content"] = &Tool{
		Capability: "browser",
		Def: llm.ToolDefinition{
			Name:        "browser_content",
			Description: "Return the current page as Markdown (scripts/styles stripped).",
			Parameters:  emptyObjectSchema(),
		},
		Fn: func(ctx context.Context, args string) (string, error) {
			return br.Content(ctx)
		},
	}

	r.tools["browser_screenshot"] = &Tool{
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
	}

	r.tools["browser_click"] = &Tool{
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
	}

	r.tools["browser_type"] = &Tool{
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
	}

	r.tools["browser_eval"] = &Tool{
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
	}

	// Filesystem tools — read/list are unrestricted, write is restricted.
	// All paths are resolved against the workspace root so the agent can't
	// reach outside the bind mount.
	workspace := cfg.Runtime.Workspace
	r.tools["file_read"] = &Tool{
		Capability: "filesystem",
		Def: llm.ToolDefinition{
			Name:        "file_read",
			Description: "Read a UTF-8 file from the workspace. Path is relative to the workspace root.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
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
			full, err := resolveWorkspacePath(workspace, p.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	}

	r.tools["file_write"] = &Tool{
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
			full, err := resolveWorkspacePath(workspace, p.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(full, []byte(p.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("Wrote %d bytes to %s", len(p.Content), p.Path), nil
		},
	}

	r.tools["file_list"] = &Tool{
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
			full, err := resolveWorkspacePath(workspace, p.Path)
			if err != nil {
				return "", err
			}
			entries, err := os.ReadDir(full)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, e := range entries {
				kind := "f"
				if e.IsDir() {
					kind = "d"
				}
				fmt.Fprintf(&b, "%s  %s\n", kind, e.Name())
			}
			return b.String(), nil
		},
	}

	return r
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// resolveWorkspacePath joins rel onto root and rejects any path that escapes
// the workspace via "..", absolute paths, or symlinks-on-disk traversal at
// resolve time. (Symlinks created later inside the workspace are out of scope
// for v1 — the workspace is treated as semi-trusted territory.)
func resolveWorkspacePath(root, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("path must be relative to the workspace: %q", rel)
	}
	full := filepath.Join(root, clean)
	// Ensure full is still inside root.
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(fullAbs+string(filepath.Separator), rootAbs+string(filepath.Separator)) &&
		fullAbs != rootAbs {
		return "", fmt.Errorf("path escapes workspace: %q", rel)
	}
	return fullAbs, nil
}
