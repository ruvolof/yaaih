// Built-in slash commands. When the user types text starting with "/" in the
// input box, the Enter handler routes it here instead of sending it to the
// agent. This file owns the dispatcher and the table of built-in commands.
//
// There are (by design) two future species of slash command: built-in
// handlers (Go code, this file) and prompt macros (markdown templates). This
// batch implements only the built-in side; "/exit" and "/help" are the
// proof-of-concept that establishes the pattern for every future built-in.
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/scripttools"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
)

// Command is a built-in slash command. Handler runs synchronously on the
// BubbleTea goroutine and returns a tea.Cmd to be executed afterwards (or nil
// when there is nothing further to do).
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Handler     func(m *Model, args string) tea.Cmd
}

// commandTable is the singleton registry of built-in commands, keyed by name
// and by every alias. Populated at package init.
var commandTable = map[string]*Command{}

// registerCommand adds c under its name and each of its aliases. Panics on a
// duplicate key, since that can only be a programming error at init time.
func registerCommand(c *Command) {
	keys := append([]string{c.Name}, c.Aliases...)
	for _, k := range keys {
		if _, exists := commandTable[k]; exists {
			panic(fmt.Sprintf("tui: duplicate slash command %q", k))
		}
		commandTable[k] = c
	}
}

// lookupCommand resolves a command by name or alias.
func lookupCommand(name string) (*Command, bool) {
	c, ok := commandTable[name]
	return c, ok
}

// splitCommand strips a single leading "/" and splits the rest into the
// command name and its argument string (everything after the first run of
// spaces). "/exit" -> ("exit", ""); "/save my-name" -> ("save", "my-name");
// "/" -> ("", "").
func splitCommand(text string) (name, args string) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "/")
	if text == "" {
		return "", ""
	}
	if i := strings.IndexAny(text, " \t"); i >= 0 {
		return text[:i], strings.TrimSpace(text[i+1:])
	}
	return text, ""
}

// sortedCommands returns the unique commands in name order, for /help and any
// other listing. Aliases share a *Command pointer, so dedup by name.
func sortedCommands() []*Command {
	seen := map[string]bool{}
	var out []*Command
	for _, c := range commandTable {
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func init() {
	registerCommand(&Command{
		Name:        "exit",
		Aliases:     []string{"quit"},
		Description: "Quit the harness.",
		Handler:     func(m *Model, args string) tea.Cmd { return tea.Quit },
	})
	registerCommand(&Command{
		Name:        "agents",
		Description: "List available agents.",
		Handler: func(m *Model, args string) tea.Cmd {
			m.appendLine(m.renderAgentsList())
			return nil
		},
	})
	registerCommand(&Command{
		Name:        "new",
		Description: "Start a new session; /new <agent> switches to another agent.",
		Handler: func(m *Model, args string) tea.Cmd {
			target := strings.TrimSpace(args)

			// No argument (or the current agent): live new session — keeps the
			// running agent and its container, just resets the conversation.
			if target == "" || target == m.currentAgent {
				if !m.requireAgent("new") {
					return nil
				}
				if m.refuseIfBusy("new") {
					return nil
				}
				id, err := m.ag.NewSession()
				if err != nil {
					m.appendLine(styleErr.Render("new session failed: " + err.Error()))
					return nil
				}
				m.resetChat()
				m.sessionStats = m.ag.SessionStats() // reset to zero for the new session
				m.globalStats = m.ag.GlobalStats()
				m.appendLine(styleMuted.Render("✦ started new session " + string(id)))
				return nil
			}

			// A different agent: validate it, then quit so the run loop tears
			// down the current stack and rebuilds for the target.
			if m.refuseIfBusy("new") {
				return nil
			}
			names, err := config.DiscoverAgents(m.agentsDir)
			if err != nil {
				m.appendLine(styleErr.Render("new: " + err.Error()))
				return nil
			}
			if !slices.Contains(names, target) {
				m.appendLine(styleErr.Render("unknown agent " + target + " — see /agents"))
				return nil
			}
			if _, err := config.Load(filepath.Join(m.agentsDir, target), config.Overrides{}); err != nil {
				m.appendLine(styleErr.Render("agent " + target + " failed to load: " + err.Error()))
				return nil
			}
			m.appendLine(styleMuted.Render("switching to agent " + target + "…"))
			m.switchTo = target
			return tea.Quit
		},
	})
	registerCommand(&Command{
		Name:        "resume",
		Description: "Resume a session by id (any agent; see /sessions all).",
		Handler: func(m *Model, args string) tea.Cmd {
			name := strings.TrimSpace(args)
			if name == "" {
				m.appendLine(styleErr.Render("usage: /resume <session> — see /sessions or /sessions all"))
				return nil
			}
			if m.refuseIfBusy("resume") {
				return nil
			}

			// A session of the current agent → live resume (no restart).
			if m.ag != nil {
				if name == string(m.ag.CurrentSession()) {
					m.appendLine(styleMuted.Render("already on session " + name))
					return nil
				}
				if _, err := os.Stat(filepath.Join(m.ag.SessionsDir(), name+".jsonl")); err == nil {
					history, err := m.ag.ResumeSession(session.ID(name))
					if err != nil {
						m.appendLine(styleErr.Render("resume failed: " + err.Error()))
						return nil
					}
					m.resetChat()
					m.prefillFromHistory(history)
					m.sessionStats = m.ag.SessionStats()
					m.globalStats = m.ag.GlobalStats()
					return nil
				}
			}

			// Otherwise find which agent owns it and switch+resume.
			owner := m.findSessionOwner(name)
			if owner == "" {
				m.appendLine(styleErr.Render("no session " + name + " found — see /sessions all"))
				return nil
			}
			m.appendLine(styleMuted.Render("resuming " + name + " on agent " + owner + "…"))
			m.switchTo = owner
			m.switchResume = name
			return tea.Quit
		},
	})
	registerCommand(&Command{
		Name:        "save",
		Description: "Rename the current session to <name>.",
		Handler: func(m *Model, args string) tea.Cmd {
			if !m.requireAgent("save") {
				return nil
			}
			name := strings.TrimSpace(args)
			if name == "" {
				m.appendLine(styleErr.Render("usage: /save <name>"))
				return nil
			}
			if m.refuseIfBusy("save") {
				return nil
			}
			if err := m.ag.RenameSession(session.ID(name)); err != nil {
				m.appendLine(styleErr.Render("save failed: " + err.Error()))
				return nil
			}
			m.appendLine(styleMuted.Render("✓ session saved as " + name))
			return nil
		},
	})
	registerCommand(&Command{
		Name:        "sessions",
		Description: "List saved sessions; /sessions all lists every agent's.",
		Handler: func(m *Model, args string) tea.Cmd {
			if strings.TrimSpace(args) == "all" {
				m.appendLine(m.renderAllSessions())
				return nil
			}
			if !m.requireAgent("sessions") {
				return nil
			}
			ids, err := session.List(m.ag.SessionsDir())
			if err != nil {
				m.appendLine(styleErr.Render("sessions: " + err.Error()))
				return nil
			}
			if len(ids) == 0 {
				m.appendLine(styleMuted.Render("no saved sessions"))
				return nil
			}
			current := m.ag.CurrentSession()
			var b strings.Builder
			for _, id := range ids {
				marker := "  "
				if id == current {
					marker = "* "
				}
				fmt.Fprintf(&b, "%s%s\n", marker, id)
			}
			m.appendLine(styleMuted.Render(strings.TrimRight(b.String(), "\n")))
			return nil
		},
	})
	registerCommand(&Command{
		Name:        "reload",
		Description: "Reload script tools from the tools directory.",
		Handler: func(m *Model, args string) tea.Cmd {
			if !m.requireAgent("reload") {
				return nil
			}
			if m.runner == nil {
				m.appendLine(styleErr.Render("reload: script tools need the sandbox runtime, which isn't running"))
				return nil
			}
			if m.busy {
				m.appendLine(styleErr.Render("reload: agent is busy — wait for the current turn to finish"))
				return nil
			}
			n, err := scripttools.Apply(m.cfg.ToolsDir, m.cfg.Runtime.Workspace, m.cfg.EnableWorkspaceTools, m.runner, m.ag.Registry())
			if err != nil {
				m.appendLine(styleErr.Render("reload failed: " + err.Error()))
				return nil
			}
			msg := fmt.Sprintf("reloaded %d script tool(s) from %s", n, m.cfg.ToolsDir)
			if n > 0 && !m.cfg.HasCapability(scripttools.Capability) {
				msg += fmt.Sprintf(" — note: agent.md lacks the %q capability, so they are not exposed to the model", scripttools.Capability)
			}
			m.appendLine(styleMuted.Render(msg))
			return nil
		},
	})
	registerCommand(&Command{
		Name:        "help",
		Description: "List available commands.",
		Handler: func(m *Model, args string) tea.Cmd {
			var b strings.Builder
			for _, c := range sortedCommands() {
				fmt.Fprintf(&b, "/%s — %s\n", c.Name, c.Description)
			}
			m.appendLine(styleMuted.Render(strings.TrimRight(b.String(), "\n")))
			return nil
		},
	})
}

// sessionsDirFor returns the sessions directory for the named agent. The
// current agent uses its live (override-aware) dir; others are read from their
// agent.md. Returns ("", false) if the agent can't be loaded.
func (m *Model) sessionsDirFor(name string) (string, bool) {
	if m.ag != nil && name == m.currentAgent {
		return m.ag.SessionsDir(), true
	}
	cfg, err := config.Load(filepath.Join(m.agentsDir, name), config.Overrides{})
	if err != nil {
		return "", false
	}
	return cfg.SessionsDir, true
}

// findSessionOwner returns the agent folder name whose sessions directory holds
// the given session id, or "" if none does.
func (m *Model) findSessionOwner(id string) string {
	names, err := config.DiscoverAgents(m.agentsDir)
	if err != nil {
		return ""
	}
	for _, n := range names {
		dir, ok := m.sessionsDirFor(n)
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); err == nil {
			return n
		}
	}
	return ""
}

// renderAllSessions lists every agent's sessions, each prefixed with its agent
// name, marking the active session.
func (m *Model) renderAllSessions() string {
	names, err := config.DiscoverAgents(m.agentsDir)
	if err != nil {
		return styleErr.Render("sessions: " + err.Error())
	}
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render("All sessions"))
	total := 0
	for _, n := range names {
		dir, ok := m.sessionsDirFor(n)
		if !ok {
			continue
		}
		ids, err := session.List(dir)
		if err != nil {
			continue
		}
		for _, id := range ids {
			marker := "  "
			if m.ag != nil && n == m.currentAgent && id == m.ag.CurrentSession() {
				marker = "* "
			}
			fmt.Fprintf(&b, "%s%-14s %s\n", marker, n, id)
			total++
		}
	}
	if total == 0 {
		return styleMuted.Render("no saved sessions across agents")
	}
	b.WriteString(styleMuted.Render("/resume <id> to resume any of these"))
	return b.String()
}
