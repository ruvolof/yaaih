package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const minimalAgent = `---
name: "Test Agent"
model: "test-model"
provider:
  base_url: "https://example.com/v1"
%s
---
You are a test agent.
`

// writeAgent writes an agent.md with the given extra frontmatter lines and
// returns its path.
func writeAgent(t *testing.T, dir, extra string) string {
	t.Helper()
	p := filepath.Join(dir, "agent.md")
	body := minimalAgent
	// crude templating: replace the %s placeholder
	body = stringsReplace(body, "%s", extra)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write agent.md: %v", err)
	}
	return p
}

func stringsReplace(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

func TestPathDefaultsRelativeToAgentDir(t *testing.T) {
	dir := t.TempDir()
	p := writeAgent(t, dir, "")

	cfg, err := Load(p, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "skills"); cfg.SkillsDir != want {
		t.Errorf("SkillsDir = %q, want %q", cfg.SkillsDir, want)
	}
	if want := filepath.Join(dir, "tools"); cfg.ToolsDir != want {
		t.Errorf("ToolsDir = %q, want %q", cfg.ToolsDir, want)
	}
	if want := filepath.Join(dir, "sessions"); cfg.SessionsDir != want {
		t.Errorf("SessionsDir = %q, want %q", cfg.SessionsDir, want)
	}
	if want := filepath.Join(dir, "workspace"); cfg.Runtime.Workspace != want {
		t.Errorf("Workspace = %q, want %q", cfg.Runtime.Workspace, want)
	}
}

func TestPathFromAgentMd(t *testing.T) {
	dir := t.TempDir()
	p := writeAgent(t, dir, "skills: \"./my-skills\"\ntools: \"./my-tools\"")

	cfg, err := Load(p, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "my-skills"); cfg.SkillsDir != want {
		t.Errorf("SkillsDir = %q, want %q", cfg.SkillsDir, want)
	}
	if want := filepath.Join(dir, "my-tools"); cfg.ToolsDir != want {
		t.Errorf("ToolsDir = %q, want %q", cfg.ToolsDir, want)
	}
}

func TestOverridesWinAndAreCWDRelative(t *testing.T) {
	dir := t.TempDir()
	// agent.md sets its own dirs; the overrides must still win.
	p := writeAgent(t, dir, "skills: \"./agent-skills\"\ntools: \"./agent-tools\"")

	cfg, err := Load(p, Overrides{
		Workspace: "ws-override",
		SkillsDir: "skills-override",
		ToolsDir:  "tools-override",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cwd, _ := os.Getwd()
	if want := filepath.Join(cwd, "skills-override"); cfg.SkillsDir != want {
		t.Errorf("SkillsDir = %q, want %q (CWD-relative override)", cfg.SkillsDir, want)
	}
	if want := filepath.Join(cwd, "tools-override"); cfg.ToolsDir != want {
		t.Errorf("ToolsDir = %q, want %q", cfg.ToolsDir, want)
	}
	if want := filepath.Join(cwd, "ws-override"); cfg.Runtime.Workspace != want {
		t.Errorf("Workspace = %q, want %q", cfg.Runtime.Workspace, want)
	}
}

func TestDiscoverAgents(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"bravo", "alpha"} {
		sub := filepath.Join(dir, n)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "agent.md"), []byte("---\nname: x\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A folder without agent.md and a loose file are both ignored.
	_ = os.MkdirAll(filepath.Join(dir, "not-an-agent"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "loose.md"), []byte("x"), 0o644)

	names, err := DiscoverAgents(dir)
	if err != nil {
		t.Fatalf("DiscoverAgents: %v", err)
	}
	if want := []string{"alpha", "bravo"}; !slices.Equal(names, want) {
		t.Errorf("got %v, want %v (sorted folders with agent.md only)", names, want)
	}

	ns, err := DiscoverAgents(filepath.Join(dir, "nope"))
	if err != nil || ns != nil {
		t.Errorf("missing dir = (%v, %v), want (nil, nil)", ns, err)
	}
}

func TestDiscoverRealAgents(t *testing.T) {
	const p = "../../agents"
	if _, err := os.Stat(p); err != nil {
		t.Skipf("agents dir not present: %v", err)
	}
	names, err := DiscoverAgents(p)
	if err != nil {
		t.Fatalf("DiscoverAgents: %v", err)
	}
	for _, want := range []string{"chatter", "devops", "tool-writer"} {
		if !slices.Contains(names, want) {
			t.Errorf("expected agent %q in %v", want, names)
		}
	}
}

func TestLoadAgentFolderMode(t *testing.T) {
	dir := t.TempDir()

	// A folder without agent.md is an error.
	if _, err := Load(dir, Overrides{}); err == nil {
		t.Fatal("Load on a folder without agent.md should error")
	}

	// With agent.md present, the folder path resolves to it and the default
	// skills/tools/workspace live inside the folder.
	writeAgent(t, dir, "") // writes <dir>/agent.md
	cfg, err := Load(dir, Overrides{})
	if err != nil {
		t.Fatalf("Load(folder): %v", err)
	}
	if filepath.Base(cfg.SourcePath) != "agent.md" {
		t.Errorf("SourcePath = %q, want .../agent.md", cfg.SourcePath)
	}
	for name, got := range map[string]string{
		"skills":    cfg.SkillsDir,
		"tools":     cfg.ToolsDir,
		"sessions":  cfg.SessionsDir,
		"workspace": cfg.Runtime.Workspace,
	} {
		if want := filepath.Join(dir, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestEnableWorkspaceToolsFlag(t *testing.T) {
	dir := t.TempDir()

	def := writeAgent(t, dir, "")
	cfg, err := Load(def, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EnableWorkspaceTools {
		t.Error("EnableWorkspaceTools should default to false")
	}

	dir2 := t.TempDir()
	on := writeAgent(t, dir2, "enable_workspace_tools: true")
	cfg2, err := Load(on, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg2.EnableWorkspaceTools {
		t.Error("EnableWorkspaceTools should be true when set in agent.md")
	}
}

func TestRealToolWriterAgentParses(t *testing.T) {
	const p = "../../agents/tool-writer"
	if _, err := os.Stat(p); err != nil {
		t.Skipf("tool-writer agent folder not present: %v", err)
	}
	cfg, err := Load(p, Overrides{})
	if err != nil {
		t.Fatalf("Load(tool-writer): %v", err)
	}
	if !cfg.EnableWorkspaceTools {
		t.Error("tool-writer should set enable_workspace_tools: true")
	}
	for _, cap := range []string{"shell", "browser", "filesystem", "scripts"} {
		if !cfg.HasCapability(cap) {
			t.Errorf("tool-writer.md missing capability %q", cap)
		}
	}
}

func TestAbsoluteOverrideUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := writeAgent(t, dir, "")
	abs := filepath.Join(t.TempDir(), "elsewhere", "skills")

	cfg, err := Load(p, Overrides{SkillsDir: abs})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SkillsDir != abs {
		t.Errorf("SkillsDir = %q, want absolute override %q unchanged", cfg.SkillsDir, abs)
	}
}

// The context budget applies to agents that never mention it — an agent.md
// written before the option existed has to be windowed too, or the failure it
// prevents is still one turn away.
func TestContextBudgetAppliesByDefault(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(writeAgent(t, dir, ""), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Options.MaxContextTokens != defaultMaxContextTokens {
		t.Errorf("MaxContextTokens = %d, want the default %d",
			cfg.Options.MaxContextTokens, defaultMaxContextTokens)
	}

	explicit, err := Load(writeAgent(t, t.TempDir(), "options:\n  max_context_tokens: -1"), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if explicit.Options.MaxContextTokens != -1 {
		t.Errorf("MaxContextTokens = %d, want the agent's own -1 (windowing off)",
			explicit.Options.MaxContextTokens)
	}
}

// Spending limits sit with the provider and model they are spent on. Left out,
// a session is unbounded, as it was before the limits existed.
func TestSpendLimitsUnderProvider(t *testing.T) {
	unset, err := Load(writeAgent(t, t.TempDir(), ""), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if unset.Provider.MaxSessionCostUSD != 0 || unset.Provider.MaxSessionTokens != 0 {
		t.Errorf("limits = $%v / %d tokens, want both unbounded by default",
			unset.Provider.MaxSessionCostUSD, unset.Provider.MaxSessionTokens)
	}

	limited, err := Load(writeAgent(t, t.TempDir(),
		"  max_session_cost_usd: 2.5\n  max_session_tokens: 400000"), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if limited.Provider.MaxSessionCostUSD != 2.5 || limited.Provider.MaxSessionTokens != 400_000 {
		t.Errorf("limits = $%v / %d tokens, want $2.5 / 400000",
			limited.Provider.MaxSessionCostUSD, limited.Provider.MaxSessionTokens)
	}
}

func TestNegativeSpendLimitsAreRejected(t *testing.T) {
	for _, line := range []string{"  max_session_cost_usd: -1", "  max_session_tokens: -1"} {
		if _, err := Load(writeAgent(t, t.TempDir(), line), Overrides{}); err == nil {
			t.Errorf("Load accepted %q, want an error", line)
		}
	}
}

// The sandbox is capped even when agent.md says nothing about it, and keeps the
// image's own user unless asked otherwise.
func TestRuntimeLimitsApplyByDefault(t *testing.T) {
	cfg, err := Load(writeAgent(t, t.TempDir(), ""), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RuntimeLimits{Memory: 2 << 30, CPUs: 2, PIDs: 1024}
	if cfg.Runtime.Limits != want {
		t.Errorf("limits = %+v, want %+v", cfg.Runtime.Limits, want)
	}
	if cfg.Runtime.RunAsHostUser {
		t.Error("RunAsHostUser = true, want it off unless asked for")
	}
}

func TestRuntimeLimitsAreRead(t *testing.T) {
	cfg, err := Load(writeAgent(t, t.TempDir(),
		"runtime:\n  run_as_host_user: true\n  limits:\n    memory: 512m\n    cpus: 1.5\n    pids: 256"), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RuntimeLimits{Memory: 512 << 20, CPUs: 1.5, PIDs: 256}
	if cfg.Runtime.Limits != want {
		t.Errorf("limits = %+v, want %+v", cfg.Runtime.Limits, want)
	}
	if !cfg.Runtime.RunAsHostUser {
		t.Error("RunAsHostUser = false, want true")
	}
}

func TestMemoryLimitAcceptsDockerSizes(t *testing.T) {
	for value, want := range map[string]ByteSize{
		"2g":         2 << 30,
		"768MB":      768 << 20,
		"1073741824": 1 << 30,
	} {
		cfg, err := Load(writeAgent(t, t.TempDir(), "runtime:\n  limits:\n    memory: "+value), Overrides{})
		if err != nil {
			t.Errorf("memory %q: Load: %v", value, err)
			continue
		}
		if cfg.Runtime.Limits.Memory != want {
			t.Errorf("memory %q = %d bytes, want %d", value, cfg.Runtime.Limits.Memory, want)
		}
	}
}

func TestInvalidRuntimeLimitsAreRejected(t *testing.T) {
	for _, limit := range []string{"memory: lots", "memory: -1g", "cpus: -1", "pids: -1"} {
		if _, err := Load(writeAgent(t, t.TempDir(), "runtime:\n  limits:\n    "+limit), Overrides{}); err == nil {
			t.Errorf("Load accepted %q, want an error", limit)
		}
	}
}
