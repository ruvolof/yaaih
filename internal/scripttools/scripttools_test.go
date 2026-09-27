package scripttools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
)

// stubRunner records the last command and returns canned output, standing in
// for a real container-backed executor.
type stubRunner struct {
	lastCmd string
	out     string
	err     error
}

func (s *stubRunner) Execute(ctx context.Context, cmd string) (string, error) {
	s.lastCmd = cmd
	return s.out, s.err
}
func (s *stubRunner) CWD() string     { return "/workspace" }
func (s *stubRunner) IsRunning() bool { return true }
func (s *stubRunner) Close() error    { return nil }

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

const validManifest = `{
  "name": "check_url",
  "description": "Fetch a URL and return its status.",
  "executable": "check_url.sh",
  "parameters": {"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}
}`

func TestLoadValidManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "check_url.sh", "#!/bin/sh\necho ok\n")
	writeFile(t, dir, "check_url.json", validManifest)

	ms, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("got %d manifests, want 1", len(ms))
	}
	m := ms[0]
	if m.Name != "check_url" || m.Executable != "check_url.sh" {
		t.Errorf("manifest = %+v", m)
	}
	if !m.isRestricted() {
		t.Errorf("restricted should default to true")
	}
	if m.Parameters == nil {
		t.Errorf("parameters not parsed")
	}
}

// A manifest in a subfolder is discovered, and its executable is rewritten
// relative to the tools root so the sandbox runs it at <mount>/<subdir>/<exe>.
func TestLoadNestedManifest(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "reconnaissance")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, sub, "check_url.sh", "#!/bin/sh\necho ok\n")
	writeFile(t, sub, "check_url.json", validManifest)

	ms, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("got %d manifests, want 1", len(ms))
	}
	if ms[0].Executable != "reconnaissance/check_url.sh" {
		t.Errorf("executable = %q, want reconnaissance/check_url.sh", ms[0].Executable)
	}
}

func TestLoadMissingExecutableRejected(t *testing.T) {
	dir := t.TempDir()
	// Manifest references check_url.sh but we never create it.
	writeFile(t, dir, "check_url.json", validManifest)
	// A second, valid tool should still load.
	writeFile(t, dir, "echo.sh", "#!/bin/sh\necho hi\n")
	writeFile(t, dir, "echo.json", `{"name":"echo","description":"e","executable":"echo.sh","parameters":{"type":"object"}}`)

	ms, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ms) != 1 || ms[0].Name != "echo" {
		t.Fatalf("got %+v, want only the valid 'echo' tool", ms)
	}
}

func TestLoadRestrictedDefaultsTrue(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "e.sh", "#!/bin/sh\n")
	writeFile(t, dir, "e.json", `{"name":"e","description":"d","executable":"e.sh","parameters":{"type":"object"}}`)
	writeFile(t, dir, "u.sh", "#!/bin/sh\n")
	writeFile(t, dir, "u.json", `{"name":"u","description":"d","executable":"u.sh","parameters":{"type":"object"},"restricted":false}`)

	ms, _ := Load(dir)
	by := map[string]Manifest{}
	for _, m := range ms {
		by[m.Name] = m
	}
	if !by["e"].isRestricted() {
		t.Errorf("e: restricted should default true")
	}
	if _, ok := by["u"]; !ok {
		t.Fatalf("u not loaded")
	}
	if v := false; by["u"].Restricted == nil || *by["u"].Restricted != v {
		t.Errorf("u: restricted should be explicit false")
	}
}

func TestLoadRejectsBadName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Bad.sh", "#!/bin/sh\n")
	writeFile(t, dir, "bad.json", `{"name":"Bad-Name!","description":"d","executable":"Bad.sh","parameters":{"type":"object"}}`)

	ms, _ := Load(dir)
	if len(ms) != 0 {
		t.Errorf("invalid tool name should be rejected, got %+v", ms)
	}
}

func TestLoadMissingDir(t *testing.T) {
	ms, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || ms != nil {
		t.Errorf("missing dir = (%v, %v), want (nil, nil)", ms, err)
	}
}

func TestRegisterAndInvoke(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "check_url.sh", "#!/bin/sh\n")
	writeFile(t, dir, "check_url.json", validManifest)
	ms, _ := Load(dir)

	runner := &stubRunner{out: "HTTP 200\n" + exitMarker + "0\n"}
	reg := agent.NewRegistry(&config.Config{}, runner, nil)
	Register(ms, ContainerToolsMount, runner, reg)

	tool, ok := reg.Lookup("check_url")
	if !ok {
		t.Fatalf("tool not registered")
	}
	if !tool.Restricted || tool.Capability != Capability {
		t.Errorf("tool gating wrong: restricted=%v capability=%q", tool.Restricted, tool.Capability)
	}

	out, err := tool.Fn(context.Background(), `{"url":"http://x"}`)
	if err != nil {
		t.Fatalf("Fn: %v", err)
	}
	if out != "HTTP 200" {
		t.Errorf("stdout = %q, want %q", out, "HTTP 200")
	}
	// Command shape: passes args via base64+temp file, runs the container path,
	// and captures an exit marker.
	if !strings.Contains(runner.lastCmd, ContainerToolsMount+"/check_url.sh") {
		t.Errorf("command missing container script path: %q", runner.lastCmd)
	}
	if !strings.Contains(runner.lastCmd, "base64 -d") || !strings.Contains(runner.lastCmd, exitMarker) {
		t.Errorf("command shape unexpected: %q", runner.lastCmd)
	}
	// The /tools mount is read-only, so no chmod is attempted there.
	if strings.Contains(runner.lastCmd, "chmod") {
		t.Errorf("read-only /tools command should not chmod: %q", runner.lastCmd)
	}
}

func TestRegisterWorkspaceToolChmods(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "check_url.sh", "#!/bin/sh\n")
	writeFile(t, dir, "check_url.json", validManifest)
	ms, _ := Load(dir)

	runner := &stubRunner{out: "ok\n" + exitMarker + "0\n"}
	reg := agent.NewRegistry(&config.Config{}, runner, nil)
	Register(ms, ContainerWorkspaceTools, runner, reg)
	tool, _ := reg.Lookup("check_url")
	if _, err := tool.Fn(context.Background(), `{"url":"http://x"}`); err != nil {
		t.Fatalf("Fn: %v", err)
	}
	if !strings.Contains(runner.lastCmd, ContainerWorkspaceTools+"/check_url.sh") {
		t.Errorf("command missing workspace script path: %q", runner.lastCmd)
	}
	// The workspace mount is writable, so the exec bit is ensured at run time.
	if !strings.Contains(runner.lastCmd, "chmod +x") {
		t.Errorf("writable workspace command should chmod: %q", runner.lastCmd)
	}
}

func TestApplyReplacesScriptToolsOnly(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	writeFile(t, dir, "check_url.sh", "#!/bin/sh\n")
	writeFile(t, dir, "check_url.json", validManifest)

	runner := &stubRunner{out: "ok\n" + exitMarker + "0\n"}
	// NewRegistry adds built-ins (e.g. file_read, capability "filesystem")
	// unconditionally; they must survive reloads.
	reg := agent.NewRegistry(&config.Config{}, runner, nil)

	registered, err := Apply(dir, ws, false, runner, reg)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(registered) != 1 {
		t.Fatalf("registered %+v, want 1 tool", registered)
	}
	if _, ok := reg.Lookup("check_url"); !ok {
		t.Fatal("check_url not registered after apply")
	}
	if _, ok := reg.Lookup("file_read"); !ok {
		t.Fatal("built-in file_read was wrongly removed by apply")
	}

	// Remove the first tool's files, add a different tool, apply again.
	os.Remove(filepath.Join(dir, "check_url.json"))
	os.Remove(filepath.Join(dir, "check_url.sh"))
	writeFile(t, dir, "ping.sh", "#!/bin/sh\n")
	writeFile(t, dir, "ping.json", `{"name":"ping","description":"d","executable":"ping.sh","parameters":{"type":"object"}}`)

	registered, err = Apply(dir, ws, false, runner, reg)
	if err != nil {
		t.Fatalf("Apply (2): %v", err)
	}
	if len(registered) != 1 {
		t.Fatalf("registered %+v, want 1 tool", registered)
	}
	if _, ok := reg.Lookup("check_url"); ok {
		t.Error("check_url should be gone after its manifest was removed and reapplied")
	}
	if _, ok := reg.Lookup("ping"); !ok {
		t.Error("ping should be registered after apply")
	}
	if _, ok := reg.Lookup("file_read"); !ok {
		t.Error("built-in file_read was wrongly removed by the second apply")
	}
}

func TestApplyFromWorkspace(t *testing.T) {
	dir := t.TempDir() // persistent tool folder (empty)
	ws := t.TempDir()  // workspace

	// The agent "wrote" a tool into <workspace>/tools.
	wsTools := filepath.Join(ws, "tools")
	if err := os.MkdirAll(wsTools, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, wsTools, "ping.sh", "#!/bin/sh\n")
	writeFile(t, wsTools, "ping.json", `{"name":"ping","description":"d","executable":"ping.sh","parameters":{"type":"object"}}`)

	runner := &stubRunner{out: "ok\n" + exitMarker + "0\n"}
	reg := agent.NewRegistry(&config.Config{}, runner, nil)

	// With fromWorkspace=false the empty persistent folder yields nothing.
	registered, err := Apply(dir, ws, false, runner, reg)
	if err != nil {
		t.Fatalf("Apply(false): %v", err)
	}
	if len(registered) != 0 {
		t.Fatalf("registered %+v from empty tool folder, want none", registered)
	}

	// With fromWorkspace=true the workspace-authored tool is picked up.
	registered, err = Apply(dir, ws, true, runner, reg)
	if err != nil {
		t.Fatalf("Apply(true): %v", err)
	}
	if len(registered) != 1 || !registered[0].Draft {
		t.Fatalf("registered %+v from workspace, want 1 draft", registered)
	}
	if _, ok := reg.Lookup("ping"); !ok {
		t.Error("workspace-authored tool 'ping' not registered with fromWorkspace=true")
	}
}

const unrestrictedManifest = `{"name":"fetch","description":"d","executable":"fetch.sh","parameters":{"type":"object"},"restricted":false}`

// A draft's manifest is written by the agent, so it cannot be the thing that
// decides whether the operator is asked. Promoted, the same manifest has been
// reviewed, and its opt-out stands.
func TestOnlyAPromotedToolMaySkipApproval(t *testing.T) {
	toolsDir := t.TempDir()
	workspace := t.TempDir()
	drafts := filepath.Join(workspace, "tools")
	if err := os.MkdirAll(drafts, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{toolsDir, drafts} {
		writeFile(t, dir, "fetch.sh", "#!/bin/sh\n")
		writeFile(t, dir, "fetch.json", unrestrictedManifest)
	}
	runner := &stubRunner{}
	registry := agent.NewRegistry(&config.Config{}, runner, nil)

	registered, err := Apply(toolsDir, workspace, false, runner, registry)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if tool, _ := registry.Lookup("fetch"); tool == nil || tool.Restricted {
		t.Errorf("promoted tool with \"restricted\": false registered as %+v, want unrestricted", tool)
	}
	if len(registered) != 1 || registered[0].Restricted || registered[0].Draft {
		t.Errorf("registered = %+v, want one unrestricted promoted tool", registered)
	}

	registered, err = Apply(toolsDir, workspace, true, runner, registry)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if tool, _ := registry.Lookup("fetch"); tool == nil || !tool.Restricted {
		t.Errorf("workspace draft with \"restricted\": false registered as %+v, want restricted", tool)
	}
	if len(registered) != 1 || !registered[0].Restricted || !registered[0].Draft {
		t.Errorf("registered = %+v, want the draft alone, restricted, in place of the tool it shadows", registered)
	}
}

func TestInvokeNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "check_url.sh", "#!/bin/sh\n")
	writeFile(t, dir, "check_url.json", validManifest)
	ms, _ := Load(dir)

	runner := &stubRunner{out: "boom: bad url\n" + exitMarker + "3\n"}
	reg := agent.NewRegistry(&config.Config{}, runner, nil)
	Register(ms, ContainerToolsMount, runner, reg)
	tool, _ := reg.Lookup("check_url")

	_, err := tool.Fn(context.Background(), `{"url":"http://x"}`)
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "script exited 3") || !strings.Contains(err.Error(), "boom: bad url") {
		t.Errorf("error = %q, want 'script exited 3: boom: bad url'", err.Error())
	}
}
