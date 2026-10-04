// Package config parses the agent.md file: a Markdown document whose YAML
// frontmatter describes the agent, its provider, runtime, capabilities, and
// safety settings, and whose body is the system prompt.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/docker/go-units"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/frontmatter"

	"github.com/ruvolof/aibitat-harness-go/internal/logging"
	"github.com/ruvolof/aibitat-harness-go/internal/skills"
)

var log = logging.For("config")

// Config is the fully-resolved agent configuration.
type Config struct {
	Name         string      `yaml:"name"`
	Model        string      `yaml:"model"`
	Provider     ProviderCfg `yaml:"provider"`
	Runtime      RuntimeCfg  `yaml:"runtime"`
	Capabilities []string    `yaml:"capabilities"`
	Safety       Safety      `yaml:"safety"`
	Options      Options     `yaml:"options"`

	// SkillsDir holds skill markdown files; ToolsDir holds script-tool
	// manifests + executables. Both are resolved to absolute paths during
	// Load: a relative value (or the default) is taken relative to the
	// agent.md directory, exactly like Runtime.Workspace. Defaults are
	// "skills" and "tools" next to the agent.md.
	SkillsDir string `yaml:"skills"`
	ToolsDir  string `yaml:"tools"`

	// SessionsDir holds this agent's transcript JSONL files, resolved the same
	// way as SkillsDir/ToolsDir. Defaults to "sessions" next to the agent.md.
	// Per-agent by default so resuming can only reach this agent's sessions.
	SessionsDir string `yaml:"sessions"`

	// EnableWorkspaceTools, when true, also loads script tools from the
	// <workspace>/tools directory in addition to ToolsDir. This lets an agent
	// author tools into its own workspace and pick them up with /reload before
	// the operator promotes them to the persistent tool folder. Default false.
	EnableWorkspaceTools bool `yaml:"enable_workspace_tools"`

	// SystemPrompt is the Markdown body following the frontmatter: the agent's
	// own prompt, which the agent composes with Skills into the one it sends.
	SystemPrompt string `yaml:"-"`

	// Skills are the enabled skills found in SkillsDir, in filename order.
	Skills []skills.Skill `yaml:"-"`

	// SourcePath is the absolute path to the agent.md that produced this Config.
	SourcePath string `yaml:"-"`
}

// Overrides carries command-line values that take precedence over the
// agent.md. An empty string means "not set" (fall back to the agent.md value,
// then the default). Override paths are resolved relative to the current
// working directory, since they are supplied at invocation time.
type Overrides struct {
	Workspace   string
	SkillsDir   string
	ToolsDir    string
	SessionsDir string
}

// ProviderCfg describes the OpenAI-compatible endpoint used for all model
// providers (DeepSeek, OpenRouter, local Ollama/LM Studio, ...).
type ProviderCfg struct {
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`

	// MaxSessionCostUSD and MaxSessionTokens cap what one session may spend.
	// Once either is reached the agent makes no further model requests in that
	// session. Tokens count input and output together, which is what is billed:
	// the conversation is re-sent on every request. The cost cap relies on the
	// provider reporting cost and holds nothing against one that does not. Zero
	// leaves the dimension unbounded.
	MaxSessionCostUSD float64 `yaml:"max_session_cost_usd"`
	MaxSessionTokens  int     `yaml:"max_session_tokens"`
}

// RuntimeCfg describes the Docker sandbox in which shell/browser tools execute.
type RuntimeCfg struct {
	Image     string        `yaml:"image"`
	Workspace string        `yaml:"workspace"`
	Limits    RuntimeLimits `yaml:"limits"`

	// RunAsHostUser runs the sandbox as the invoking user's uid:gid instead of
	// root, so files the agent writes into the workspace belong to the operator
	// on the host. The agent loses root inside the container with it: no
	// apt-get, nothing that needs raw sockets.
	RunAsHostUser bool `yaml:"run_as_host_user"`
}

// RuntimeLimits caps what the sandbox container may take from the host. A zero
// field takes its default; there is no way to lift a limit entirely, only to
// raise it.
type RuntimeLimits struct {
	Memory ByteSize `yaml:"memory"`
	CPUs   float64  `yaml:"cpus"`

	// PIDs counts threads as well as processes. Chromium alone runs a few
	// hundred, so the default leaves room for the browser and still stops a
	// fork bomb.
	PIDs int64 `yaml:"pids"`
}

// ByteSize is a size in bytes, written in agent.md the way `docker run
// --memory` takes it: "2g", "512m", or a bare number of bytes.
type ByteSize int64

// UnmarshalText parses a human-readable size such as "2g" or "512MB".
func (size *ByteSize) UnmarshalText(text []byte) error {
	parsed, err := units.RAMInBytes(string(text))
	if err != nil {
		return fmt.Errorf("invalid size %q: %w", text, err)
	}
	*size = ByteSize(parsed)
	return nil
}

// Safety controls human-in-the-loop gating.
type Safety struct {
	ApprovalRequired bool `yaml:"approval_required"`
}

// Options is the bag of behavioural knobs for the agent loop.
type Options struct {
	MaxToolIterations int `yaml:"max_tool_iterations"`
	CommandTimeout    int `yaml:"command_timeout"` // seconds

	// MaxToolResultBytes bounds how much of a tool's output is kept in the
	// conversation. History is re-sent on every subsequent request, so this is
	// the ceiling on what one tool call costs for the rest of the session.
	// Output above it is truncated head-and-tail, and the full text is written
	// to the workspace for the agent to grep.
	MaxToolResultBytes int `yaml:"max_tool_result_bytes"`

	// MaxFileReadLines is the default page size for file_read when the model
	// does not ask for one.
	MaxFileReadLines int `yaml:"max_file_read_lines"`

	// MaxContextTokens is the ceiling on one request: system prompt, tool
	// definitions, and conversation together. Above it the oldest exchanges stop
	// being sent. Leave headroom below the model's real context window for the
	// reply. Zero or less sends the whole conversation however large it grows.
	MaxContextTokens int `yaml:"max_context_tokens"`

	// BrowserUserAgent overrides the User-Agent string the sandboxed Chromium
	// advertises. Left empty, one is derived from the browser's own version so
	// the two cannot disagree.
	BrowserUserAgent string `yaml:"browser_user_agent"`

	// BrowserTimeout caps navigation, clicks, typing, and waits in the browser
	// tools. It bounds a single browser operation, not a whole turn, so it is
	// shorter than CommandTimeout.
	BrowserTimeout int `yaml:"browser_timeout"` // seconds
}

// Defaults applied after parsing if the agent.md leaves fields blank.
const (
	defaultRuntimeImage  = "aibitat-harness-runtime:latest"
	defaultWorkspace     = "./workspace"
	defaultSkillsDir     = "skills"
	defaultToolsDir      = "tools"
	defaultSessionsDir   = "sessions"
	defaultMaxIterations = 25
	defaultCmdTimeoutSec = 120

	// Enough for Chromium with a few tabs alongside a build or a test run; low
	// enough that a runaway process is stopped by the sandbox rather than by
	// the host's OOM killer, which picks its own victim.
	defaultRuntimeMemory = ByteSize(2 << 30)
	defaultRuntimeCPUs   = 2.0
	defaultRuntimePIDs   = 1024

	// Long enough for a slow page to reach DOMContentLoaded on a bad
	// connection, short enough that a selector that will never match gives the
	// turn back instead of stalling it.
	defaultBrowserTimeoutSec = 30

	// 32 KiB is roughly 8k tokens: enough for a stack trace, a test run, or a
	// directory listing to arrive whole, small enough that a handful of them do
	// not crowd out the conversation.
	defaultMaxToolResultBytes = 32 << 10
	defaultMaxFileReadLines   = 500

	// Comfortably inside every model the harness is pointed at today, and low
	// enough to keep the per-turn bill of a long autonomous run bounded: the
	// conversation is re-sent on every iteration, so this number is paid again
	// and again. Agents on a smaller model must lower it.
	defaultMaxContextTokens = 64_000
)

// Load reads, parses, and validates an agent, applying any command-line
// overrides on top of the file's values. path is normally an agent folder
// containing an agent.md; a direct path to an agent.md file is also accepted.
// Skills, tools, and workspace default to subdirectories of the agent folder.
func Load(path string, ov Overrides) (*Config, error) {
	abs, err := resolveAgentFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read agent file: %w", err)
	}

	md := goldmark.New(
		goldmark.WithExtensions(&frontmatter.Extender{}),
	)
	ctx := parser.NewContext()
	doc := md.Parser().Parse(text.NewReader(raw), parser.WithContext(ctx))

	fm := frontmatter.Get(ctx)
	if fm == nil {
		return nil, errors.New("agent.md is missing YAML frontmatter")
	}

	cfg := &Config{}
	if err := fm.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode frontmatter: %w", err)
	}

	// Extract the Markdown body (everything after the frontmatter block).
	cfg.SystemPrompt = strings.TrimSpace(extractBody(raw))
	cfg.SourcePath = abs
	_ = doc // doc retained only to anchor ctx; goldmark API does not expose body directly

	// Resolve paths: CLI overrides win over agent.md values, which win over
	// defaults. Overrides are taken relative to CWD; agent.md values and
	// defaults relative to the agent.md directory.
	if err := applyOverrides(cfg, ov); err != nil {
		return nil, err
	}
	applyDefaults(cfg)
	resolveRelativePaths(cfg, filepath.Dir(abs))

	// Absence of the skills directory is not an error.
	cfg.Skills, err = skills.Load(cfg.SkillsDir)
	if err != nil {
		log.Warn("skills load partial failure", "err", err)
	}
	onDemand := 0
	for _, skill := range cfg.Skills {
		if skill.OnDemand() {
			onDemand++
		}
	}
	log.Info("skills loaded", "count", len(cfg.Skills), "on_demand", onDemand, "dir", cfg.SkillsDir)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyOverrides sets any path supplied on the command line, resolved to an
// absolute path relative to the current working directory. Empty override
// values are left untouched so the agent.md value (or default) applies.
func applyOverrides(c *Config, ov Overrides) error {
	for _, o := range []struct {
		val string
		dst *string
	}{
		{ov.Workspace, &c.Runtime.Workspace},
		{ov.SkillsDir, &c.SkillsDir},
		{ov.ToolsDir, &c.ToolsDir},
		{ov.SessionsDir, &c.SessionsDir},
	} {
		if o.val == "" {
			continue
		}
		abs, err := filepath.Abs(o.val)
		if err != nil {
			return fmt.Errorf("resolve override path %q: %w", o.val, err)
		}
		*o.dst = abs
	}
	return nil
}

// agentFileName is the fixed name of the agent definition inside an agent folder.
const agentFileName = "agent.md"

// DiscoverAgents returns the names of agent folders directly under dir — the
// subdirectories that contain an agent.md — sorted alphabetically. A missing
// dir returns (nil, nil); it is not an error.
func DiscoverAgents(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read agents dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), agentFileName)); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// resolveAgentFile turns the --agent path into an absolute path to the agent.md
// file. When path is a directory it must contain agent.md (failing otherwise);
// when path is a file it is used directly.
func resolveAgentFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve agent path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("agent path: %w", err)
	}
	if info.IsDir() {
		md := filepath.Join(abs, agentFileName)
		if _, err := os.Stat(md); err != nil {
			return "", fmt.Errorf("no %s in agent folder %s", agentFileName, abs)
		}
		return md, nil
	}
	return abs, nil
}

// extractBody returns the raw Markdown content after the closing `---` of the
// YAML frontmatter. If no frontmatter delimiters are found, the whole input is
// returned (Load's caller will already have failed on missing frontmatter).
func extractBody(raw []byte) string {
	open := []byte("---")
	if !bytes.HasPrefix(raw, open) {
		return string(raw)
	}
	// Skip the opening delimiter line.
	rest := raw[len(open):]
	// Find the closing delimiter.
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return string(raw)
	}
	body := rest[idx+len("\n---"):]
	// Drop the rest of the closing line.
	if nl := bytes.IndexByte(body, '\n'); nl >= 0 {
		body = body[nl+1:]
	}
	return string(body)
}

func applyDefaults(c *Config) {
	if c.Runtime.Image == "" {
		c.Runtime.Image = defaultRuntimeImage
	}
	if c.Runtime.Workspace == "" {
		c.Runtime.Workspace = defaultWorkspace
	}
	if c.Runtime.Limits.Memory == 0 {
		c.Runtime.Limits.Memory = defaultRuntimeMemory
	}
	if c.Runtime.Limits.CPUs == 0 {
		c.Runtime.Limits.CPUs = defaultRuntimeCPUs
	}
	if c.Runtime.Limits.PIDs == 0 {
		c.Runtime.Limits.PIDs = defaultRuntimePIDs
	}
	if c.SkillsDir == "" {
		c.SkillsDir = defaultSkillsDir
	}
	if c.ToolsDir == "" {
		c.ToolsDir = defaultToolsDir
	}
	if c.SessionsDir == "" {
		c.SessionsDir = defaultSessionsDir
	}
	if c.Options.MaxToolIterations == 0 {
		c.Options.MaxToolIterations = defaultMaxIterations
	}
	if c.Options.CommandTimeout == 0 {
		c.Options.CommandTimeout = defaultCmdTimeoutSec
	}
	if c.Options.MaxToolResultBytes == 0 {
		c.Options.MaxToolResultBytes = defaultMaxToolResultBytes
	}
	if c.Options.MaxFileReadLines == 0 {
		c.Options.MaxFileReadLines = defaultMaxFileReadLines
	}
	if c.Options.MaxContextTokens == 0 {
		c.Options.MaxContextTokens = defaultMaxContextTokens
	}
	if c.Options.BrowserTimeout == 0 {
		c.Options.BrowserTimeout = defaultBrowserTimeoutSec
	}
}

// resolveRelativePaths turns the workspace, skills, tools, and sessions paths
// into absolute paths relative to the agent.md's directory, so the harness can
// be invoked from anywhere. Values already made absolute by an override are
// left unchanged.
func resolveRelativePaths(c *Config, agentDir string) {
	for _, p := range []*string{&c.Runtime.Workspace, &c.SkillsDir, &c.ToolsDir, &c.SessionsDir} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(agentDir, *p)
		}
	}
}

// APIKey reads the API key from the environment variable named by the
// provider config. Returns "" if no env var is configured (acceptable for
// local providers like Ollama).
func (c *Config) APIKey() string {
	if c.Provider.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.Provider.APIKeyEnv)
}

// HasCapability reports whether the agent has been granted the given capability.
func (c *Config) HasCapability(name string) bool {
	for _, cap := range c.Capabilities {
		if cap == name {
			return true
		}
	}
	return false
}

// Validate enforces required fields and recognised enum values.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("agent.md: `name` is required")
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("agent.md: `model` is required")
	}
	if strings.TrimSpace(c.Provider.BaseURL) == "" {
		return errors.New("agent.md: `provider.base_url` is required")
	}
	if c.Provider.MaxSessionCostUSD < 0 {
		return errors.New("agent.md: `provider.max_session_cost_usd` cannot be negative (0 means no limit)")
	}
	if c.Provider.MaxSessionTokens < 0 {
		return errors.New("agent.md: `provider.max_session_tokens` cannot be negative (0 means no limit)")
	}
	if c.Runtime.Limits.CPUs < 0 {
		return errors.New("agent.md: `runtime.limits.cpus` cannot be negative")
	}
	if c.Runtime.Limits.PIDs < 0 {
		return errors.New("agent.md: `runtime.limits.pids` cannot be negative")
	}
	if c.Provider.APIKeyEnv != "" && os.Getenv(c.Provider.APIKeyEnv) == "" {
		// Not fatal — local providers may legitimately omit auth — but warn.
		log.Warn("api_key_env is set but the environment variable is empty",
			"api_key_env", c.Provider.APIKeyEnv,
		)
	}
	return nil
}
