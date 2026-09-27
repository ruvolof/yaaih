// aibitat-harness-go: a high-performance stateful agent runtime with a TUI front-end.
// The host process owns the TUI and the LLM client; every tool that touches
// the world (shell, browser, filesystem) executes inside a sandboxed Docker
// container managed for the lifetime of the session.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/browser"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/docker"
	"github.com/ruvolof/aibitat-harness-go/internal/dotenv"
	"github.com/ruvolof/aibitat-harness-go/internal/executor"
	"github.com/ruvolof/aibitat-harness-go/internal/governor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/logging"
	"github.com/ruvolof/aibitat-harness-go/internal/scripttools"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
	"github.com/ruvolof/aibitat-harness-go/internal/tui"
)

func main() {
	agentPath := flag.String("agent", "", "path to a single agent folder (contains agent.md); selected on startup")
	agentsRoot := flag.String("agents", "", "path to a folder of agent folders; starts with no agent selected (pick with /new)")
	logLevel := flag.String("log-level", "info", "log verbosity: debug, info, warn, error")
	resumeID := flag.String("resume", "", "resume a prior session by id (from the agent's sessions/ folder)")
	newSession := flag.Bool("new-session", true, "persist this run to the agent's sessions/ folder (ignored when --resume is set)")
	workspaceDir := flag.String("workspace", "", "override the workspace directory from agent.md")
	skillsDir := flag.String("skills-dir", "", "override the skills directory from agent.md")
	toolsDir := flag.String("tools-dir", "", "override the tools directory from agent.md")
	sessionsDir := flag.String("sessions-dir", "", "override the sessions directory from agent.md")
	flag.Parse()

	// Open the per-session log file as early as possible so subsequent stderr
	// writes (including Go's runtime panic dumps, where supported) survive the
	// TUI taking over the terminal. A failure here is non-fatal — we keep
	// going with logging disabled.
	logSess, logErr := logging.Init(".", *logLevel)
	if logErr != nil {
		fmt.Fprintln(os.Stderr, "warning: could not open log file:", logErr)
	}
	defer func() {
		if logSess != nil {
			_ = logSess.Close()
		}
	}()

	log := logging.For("main")

	// Populate the environment from a .env file (working directory first, then
	// next to the binary) before any config is read, so `provider.api_key_env`
	// resolves without the operator exporting the key by hand. Already-exported
	// variables take precedence; a missing file is not an error.
	if path, err := dotenv.LoadDefault(); err != nil {
		log.Warn("could not load .env", "err", err)
	} else if path != "" {
		log.Info("loaded environment file", "path", path)
	}

	// Recover from panics in the main goroutine. The logging package records
	// the full stack to the log file; we then print a brief summary that
	// points the user at the log path.
	defer func() {
		if r := recover(); r != nil {
			log.Error("main panic", "value", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			fmt.Fprintf(os.Stderr, "panic: %v\n", r)
			if p := logSess.Path(); p != "" {
				fmt.Fprintln(os.Stderr, "see log:", p)
			}
			_ = logSess.Close() // os.Exit skips deferred Close
			os.Exit(1)
		}
	}()

	overrides := config.Overrides{
		Workspace:   *workspaceDir,
		SkillsDir:   *skillsDir,
		ToolsDir:    *toolsDir,
		SessionsDir: *sessionsDir,
	}
	root, initial := resolveAgentSelection(*agentPath, *agentsRoot)
	if err := run(root, initial, *resumeID, *newSession, overrides); err != nil {
		log.Error("fatal", "err", err)
		fmt.Fprintln(os.Stderr, "fatal:", err)
		if p := logSess.Path(); p != "" {
			fmt.Fprintln(os.Stderr, "see log:", p)
		}
		_ = logSess.Close()
		os.Exit(1)
	}
}

// resolveAgentSelection turns the --agent / --agents flags into the directory
// agents are discovered in (root) and the agent folder selected at startup
// (initial, "" = start with no agent). --agents takes precedence and starts
// agent-less; --agent selects one and makes its siblings switchable; with
// neither, the default chatter agent is selected.
func resolveAgentSelection(agentPath, agentsRoot string) (root, initial string) {
	switch {
	case agentsRoot != "":
		return agentsRoot, ""
	case agentPath != "":
		return filepath.Dir(agentPath), filepath.Base(agentPath)
	default:
		return "./agents", "chatter"
	}
}

// run owns the agent lifecycle loop. It builds the selected agent's full stack
// (runtime, provider, session, agent, tools), runs the TUI, and — when the
// operator picks a different agent with /new <agent> — tears the stack down and
// rebuilds it for the new agent. The loop exits when the TUI quits without a
// switch target (/exit or Ctrl+C).
func run(agentsRoot, initialAgent, resumeID string, newSession bool, overrides config.Overrides) error {
	log := logging.For("main")

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ctrl+C / SIGTERM cleanly tears down container + binary.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer logging.Recover("main.signal-watcher")
		sig := <-sigs
		log.Info("signal received, shutting down", "signal", sig.String())
		cancel()
	}()

	// globalGov accumulates token/cost usage across the whole run — it is shared
	// by every agent the loop builds, so it survives agent switches. The
	// per-session governor is created fresh inside each build.
	globalGov := governor.New("")

	current := initialAgent
	resume := resumeID // resume id for the next build (launch agent only at first)
	first := true
	for {
		var rt *agentRuntime
		if current != "" {
			var err error
			rt, err = buildAgentRuntime(rootCtx, filepath.Join(agentsRoot, current), resume, newSession, overrides, globalGov)
			if err != nil {
				if first {
					return err
				}
				// A switch target failed to build after the previous agent was
				// torn down. Drop to the agent-less picker so the operator can
				// choose another rather than crashing the harness.
				log.Error("failed to build agent, returning to picker", "agent", current, "err", err)
				current, resume = "", ""
				continue
			}
		}
		first = false

		var cfg *config.Config
		var ag *agent.Agent
		var runner executor.Executor
		if rt != nil {
			cfg, ag, runner = rt.cfg, rt.ag, rt.runner
		}

		log.Info("starting TUI event loop", "agent", current)
		model := tui.New(rootCtx, cfg, ag, runner, agentsRoot, current)
		// WithMouseCellMotion enables wheel events (and click/drag, which we
		// ignore). This means the terminal no longer handles native click-drag
		// selection in the app window — that's why we expose the clipboard via
		// ctrl+y / ctrl+t hotkeys instead.
		p := tea.NewProgram(model,
			tea.WithAltScreen(),
			tea.WithContext(rootCtx),
			tea.WithMouseCellMotion(),
		)
		final, perr := p.Run()
		if rt != nil {
			rt.teardown()
		}
		if perr != nil {
			return fmt.Errorf("tui: %w", perr)
		}

		target := ""
		if fm, ok := final.(*tui.Model); ok {
			target = fm.SwitchTarget()
			resume = fm.SwitchResume() // "" for a plain switch, an id for a cross-agent resume
		}
		if target == "" {
			log.Info("TUI exited cleanly, no agent switch requested")
			return nil
		}
		log.Info("switching agent", "from", current, "to", target)
		current = target
	}
}

// agentRuntime bundles a fully wired agent plus a teardown that releases all of
// its resources (session footer, browser, executor, container).
type agentRuntime struct {
	cfg      *config.Config
	ag       *agent.Agent
	runner   executor.Executor
	teardown func()
}

// buildAgentRuntime loads the agent at agentDir and wires up its runtime,
// provider, session, agent, and script tools. On any failure it releases
// whatever it has already started before returning.
func buildAgentRuntime(ctx context.Context, agentDir, resumeID string, newSession bool, overrides config.Overrides, globalGov *governor.Governor) (*agentRuntime, error) {
	log := logging.For("main")

	cfg, err := config.Load(agentDir, overrides)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	log.Info("agent loaded",
		"name", cfg.Name,
		"model", cfg.Model,
		"provider", cfg.Provider.BaseURL,
		"api_key_set", cfg.APIKey() != "",
		"max_session_cost_usd", cfg.Provider.MaxSessionCostUSD,
		"max_session_tokens", cfg.Provider.MaxSessionTokens,
		"capabilities", cfg.Capabilities,
		"approval_required", cfg.Safety.ApprovalRequired,
		"runtime_image", cfg.Runtime.Image,
		"workspace", cfg.Runtime.Workspace,
		"skills_dir", cfg.SkillsDir,
		"tools_dir", cfg.ToolsDir,
		"sessions_dir", cfg.SessionsDir,
		"max_iter", cfg.Options.MaxToolIterations,
		"cmd_timeout_sec", cfg.Options.CommandTimeout,
		"max_context_tokens", cfg.Options.MaxContextTokens,
	)

	// 1. Bring up the runtime sandbox only when the agent declares runtime tools.
	var runner executor.Executor
	var br *browser.Browser
	dockerCleanup := func() {}
	if runtimeRequired(cfg) {
		log.Info("runtime required, starting sandbox")
		runner, br, dockerCleanup, err = setupRuntime(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("runtime: %w", err)
		}
	} else {
		log.Info("runtime not required (no shell/browser/filesystem/scripts capability)")
	}

	// 2. LLM provider, governor, gatekeeper.
	prov := llm.NewOpenAICompat(cfg.Provider.BaseURL, cfg.APIKey(), cfg.Model)
	gov := governor.New(cfg.Model)
	gov.SetLimits(governor.Limits{
		MaxCostUSD: cfg.Provider.MaxSessionCostUSD,
		MaxTokens:  cfg.Provider.MaxSessionTokens,
	})
	gate := agent.NewGatekeeper(cfg.Safety.ApprovalRequired)

	// 3. Session persistence: resume a prior transcript or start a new one. When
	// resuming, seed the session governor with the transcript's accumulated
	// usage so the per-session cost continues where it left off.
	sessWriter, seed, err := setupSession(resumeID, newSession, cfg, cfg.SessionsDir)
	if err != nil {
		dockerCleanup()
		if runner != nil {
			runner.Close()
		}
		if br != nil {
			br.Close()
		}
		return nil, fmt.Errorf("session: %w", err)
	}
	if len(seed.Messages) > 0 {
		gov.Seed(seed.Usage.InputTokens, seed.Usage.OutputTokens, seed.Usage.Cost, seed.Usage.CostAvailable)
	}

	// 4. Agent.
	ag := agent.New(cfg, prov, gov, globalGov, runner, br, gate, sessWriter, seed, cfg.SessionsDir)

	// 5. Script tools — only when the runtime is up (they execute in the sandbox).
	if runner != nil {
		if registered, lerr := scripttools.Apply(cfg.ToolsDir, cfg.Runtime.Workspace, cfg.EnableWorkspaceTools, runner, ag.Registry()); lerr != nil {
			log.Warn("script tools load failed", "err", lerr, "dir", cfg.ToolsDir)
		} else if len(registered) > 0 {
			log.Info("script tools loaded", "count", len(registered), "workspace_tools", cfg.EnableWorkspaceTools)
		}
	}

	teardown := func() {
		ag.Close() // writes the session footer and stops the gatekeeper forwarder
		if br != nil {
			br.Close()
		}
		if runner != nil {
			runner.Close()
		}
		dockerCleanup()
	}
	return &agentRuntime{cfg: cfg, ag: ag, runner: runner, teardown: teardown}, nil
}

// orphanSweep runs the orphan-container cleanup once per harness run. Each
// agent switch removes its own container on teardown, and whatever a crashed
// harness leaves behind in the meantime is caught by the next launch.
var orphanSweep sync.Once

// setupRuntime brings up the Docker sandbox: verifies the runtime image is
// present, starts the session container, and wires the executor and browser
// to it. Returns a cleanup func that stops and removes the container.
//
// The runtime image must be built ahead of time via `./build.sh image` or
// `.\build.ps1 image`; setup fails if it isn't present locally.
func setupRuntime(ctx context.Context, cfg *config.Config) (executor.Executor, *browser.Browser, func(), error) {
	log := logging.For("main")
	orphanSweep.Do(func() { docker.CleanupOrphans(ctx, os.Stderr) })

	rt, err := docker.New(ctx, docker.Options{
		Image:     cfg.Runtime.Image,
		Workspace: cfg.Runtime.Workspace,
		ToolsDir:  cfg.ToolsDir,
		Limits: docker.Limits{
			MemoryBytes: int64(cfg.Runtime.Limits.Memory),
			CPUs:        cfg.Runtime.Limits.CPUs,
			PIDs:        cfg.Runtime.Limits.PIDs,
		},
		RunAsHostUser:  cfg.Runtime.RunAsHostUser,
		ProgressWriter: os.Stderr,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := rt.Start(ctx); err != nil {
		_ = rt.Close()
		return nil, nil, nil, err
	}
	if err := rt.WaitHealthy(ctx, 10*time.Second); err != nil {
		_ = rt.Stop(ctx)
		_ = rt.Close()
		return nil, nil, nil, err
	}

	cmdTimeout := time.Duration(cfg.Options.CommandTimeout) * time.Second
	runner, err := executor.NewDockerExecutor(ctx, rt.Client(), rt.ContainerID(), cmdTimeout)
	if err != nil {
		_ = rt.Stop(ctx)
		_ = rt.Close()
		return nil, nil, nil, err
	}

	screenshotsDir := filepath.Join(cfg.Runtime.Workspace, "screenshots")
	br := browser.New(browser.Options{
		CDPEndpoint:    rt.CDPEndpoint(),
		DockerClient:   rt.Client(),
		ContainerID:    rt.ContainerID(),
		ScreenshotsDir: screenshotsDir,
		SupportsVision: cfg.Options.SupportsVision,
		UserAgent:      cfg.Options.BrowserUserAgent,
		Timeout:        time.Duration(cfg.Options.BrowserTimeout) * time.Second,
	})

	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = rt.Stop(stopCtx)
		if err := rt.Close(); err != nil {
			log.Warn("docker client close failed", "err", err)
		}
	}
	return runner, br, cleanup, nil
}

// setupSession wires up transcript persistence. When resumeID is set it loads
// the prior transcript (its messages, accumulated usage, and loaded skills) and
// reopens that file for appending. When resumeID is empty and newSession is
// true it creates a fresh session file with a header. Returns nil writer and an
// empty transcript when persistence is disabled.
//
// sessionsDir is the directory holding this agent's transcripts (cfg.SessionsDir,
// default <agent folder>/sessions).
func setupSession(resumeID string, newSession bool, cfg *config.Config, sessionsDir string) (*session.Writer, session.Transcript, error) {
	log := logging.For("main")

	if resumeID != "" {
		seed, err := session.LoadTranscript(sessionsDir, session.ID(resumeID))
		if err != nil {
			return nil, session.Transcript{}, fmt.Errorf("resume %s: %w", resumeID, err)
		}
		w, err := session.OpenForAppend(sessionsDir, session.ID(resumeID))
		if err != nil {
			return nil, session.Transcript{}, fmt.Errorf("reopen %s: %w", resumeID, err)
		}
		log.Info("session resumed",
			"id", resumeID,
			"messages", len(seed.Messages),
			"skills", seed.Skills,
			"header_model", seed.Header.Model,
		)
		return w, seed, nil
	}

	if !newSession {
		log.Info("session persistence disabled (--new-session=false)")
		return nil, session.Transcript{}, nil
	}

	id := session.NewID()
	w, err := session.NewWriter(sessionsDir, session.Header{
		ID:        id,
		Started:   time.Now().UTC().Format(time.RFC3339),
		AgentPath: cfg.SourcePath,
		Model:     cfg.Model,
		Provider:  cfg.Provider.BaseURL,
	})
	if err != nil {
		return nil, session.Transcript{}, fmt.Errorf("create session: %w", err)
	}
	log.Info("session started", "id", id, "path", filepath.Join(sessionsDir, string(id)+".jsonl"))
	return w, session.Transcript{}, nil
}

func runtimeRequired(cfg *config.Config) bool {
	for _, c := range cfg.Capabilities {
		switch c {
		case "shell", "browser", "filesystem", scripttools.Capability:
			return true
		}
	}
	return false
}
