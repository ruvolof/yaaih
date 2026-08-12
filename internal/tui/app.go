// Root BubbleTea model: chat viewport on the left, stats sidebar on the right,
// input bar across the bottom, and an approval modal overlay when a
// restricted tool needs operator confirmation.
package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/ruvolof/aibitat-harness-go/internal/agent"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/executor"
	"github.com/ruvolof/aibitat-harness-go/internal/governor"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var log = logging.For("tui")

// ansiCSI matches CSI sequences (everything lipgloss emits for colour, bold,
// reset, etc.). Used to strip styling when copying text to the clipboard so
// the receiving app gets plain text.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// toastTTL is how long a "copied!" hint lingers in the sidebar before clearing.
const toastTTL = 2 * time.Second

// busyTickInterval is how often the streaming indicator's dots advance.
// Long enough that the indicator looks calm, short enough that the user
// perceives it as live.
const busyTickInterval = 300 * time.Millisecond

// renderTickInterval is how often streamed assistant deltas are flushed into
// the chat viewport. Deltas arrive faster than a terminal can usefully
// repaint (an OpenAI-style stream is often 5–30 chunks per second per token);
// coalescing them into ~renderTickInterval-spaced repaints keeps the render
// cost roughly constant per second of streaming rather than per delta.
const renderTickInterval = 50 * time.Millisecond

// Model is the root BubbleTea model.
type Model struct {
	// cfg, ag, and runner are nil in the "agent-less" state (multi-agent mode
	// before the operator has selected an agent with /new).
	cfg    *config.Config
	ag     *agent.Agent
	runner executor.Executor // may be nil when no runtime sandbox is up
	ctx    context.Context

	// agentsDir is the directory agents are discovered in (for /agents and
	// /new <agent>); currentAgent is the selected agent's folder name ("" when
	// agent-less). switchTo is set by /new <agent> to request the run loop tear
	// down and rebuild for a different agent; the program then quits and the
	// loop reads it via SwitchTarget.
	agentsDir    string
	currentAgent string
	switchTo     string
	switchResume string // session id to resume after switching (cross-agent /resume)

	width  int
	height int

	chat      viewport.Model
	chatBuf   strings.Builder
	pendingAs strings.Builder // assistant content currently being streamed

	// wrappedBody is chatBuf pre-wrapped to wrapWidth. Kept in sync
	// incrementally by appendLine so the streaming hot path does not re-wrap
	// the whole transcript on every AssistantDeltaMsg; a full rebuild happens
	// only in rebuildWrapped, which is called from renderChat when the width
	// changes (WindowSizeMsg / relayout). This is the fix for the O(n²) chat
	// rendering described in REVIEW.md B1.
	wrappedBody strings.Builder
	wrapWidth   int // width wrappedBody was computed at; 0 = uninitialized

	// renderPending / renderTicking coalesce bursts of AssistantDeltaMsg into
	// one repaint every renderTickInterval instead of one per chunk. Cleared
	// when a tick fires or when commitAssistant forces a final flush.
	renderPending bool
	renderTicking bool

	// userScrolled tracks whether the user has manually scrolled the chat
	// viewport away from the bottom. When true, new content (streaming
	// deltas, tool results, etc.) is appended without snapping the view
	// back to the bottom — they get to read history undisturbed. Reset to
	// false when the viewport returns to bottom (Ctrl+End, PgDn to bottom,
	// or wheel-down to bottom) or when the user sends a new message.
	userScrolled bool

	// busy is true while a turn is in flight (between the user pressing
	// Enter and AssistantDoneMsg / ErrorMsg). busyFrame advances every
	// busyTickInterval via busyTickMsg and drives the dot animation in
	// renderStatus.
	busy      bool
	busyFrame int

	// cancelTurn stops the in-flight turn: it cancels the context handed to
	// Agent.Send, which unwinds the LLM stream and any running tool. Set when a
	// turn starts, cleared when one ends. Only esc calls it, and only while
	// busy — a stale non-nil value between turns is harmless.
	cancelTurn context.CancelFunc

	// lastAssistant holds the plain-text version of the most recent completed
	// assistant message so ctrl+y can copy it without depending on chatBuf's
	// styling. Updated in commitAssistant.
	lastAssistant string

	// toast is a short status hint shown in the sidebar (e.g. "copied 412
	// chars"). Cleared by a tea.Tick scheduled with toastTTL.
	toast        string
	toastVersion int // monotonic; lets stale Ticks find their toast already replaced

	input textinput.Model

	sessionStats governor.Stats // current session's token/cost usage
	globalStats  governor.Stats // whole-run usage (accumulates across agent switches)
	cwd          string

	// contextTokens is what the last request cost against contextBudget. Shown
	// in the sidebar so the operator sees the window filling in time to
	// /compact rather than after the harness has started dropping history.
	// contextWarned keeps the transcript warning to one per filling.
	contextTokens int
	contextBudget int
	contextWarned bool

	pendingApproval *agent.ApprovalRequest
}

// toastExpiredMsg is delivered by a tea.Tick to clear an old toast. The
// version is checked against the model's current toastVersion so a stale
// timer can't blank a newer message.
type toastExpiredMsg struct{ version int }

// busyTickMsg drives the streaming-indicator animation. Scheduled by
// busyTick(); the handler re-arms a new tick only while m.busy is true so
// the chain self-terminates once a turn completes.
type busyTickMsg struct{}

func busyTick() tea.Cmd {
	return tea.Tick(busyTickInterval, func(time.Time) tea.Msg { return busyTickMsg{} })
}

// renderTickMsg is delivered by a tea.Tick to flush any pending streamed
// deltas into the chat viewport. See renderTickInterval.
type renderTickMsg struct{}

func renderTick() tea.Cmd {
	return tea.Tick(renderTickInterval, func(time.Time) tea.Msg { return renderTickMsg{} })
}

// New constructs the TUI model. Pass the cancellable context the program is
// using; agent.Send is invoked off the BubbleTea loop with the same context.
//
// Returns a *Model so the same allocation lives across every Update/View
// pass. Returning Model by value would cause BubbleTea to copy the struct
// each iteration, and the strings.Builder fields cache a self-pointer via
// noescape(unsafe.Pointer); a stale copy of that pointer is what triggered a
// "found bad pointer in Go heap" GC crash on the first chat message.
func New(ctx context.Context, cfg *config.Config, ag *agent.Agent, runner executor.Executor, agentsDir, currentAgent string) *Model {
	ti := textinput.New()
	ti.Placeholder = "Type your message... (Ctrl+C to quit)"
	ti.Focus()
	ti.CharLimit = 4096
	ti.Width = 80

	vp := viewport.New(80, 20)
	vp.SetContent("")

	m := &Model{
		cfg:          cfg,
		ag:           ag,
		runner:       runner,
		ctx:          ctx,
		agentsDir:    agentsDir,
		currentAgent: currentAgent,
		chat:         vp,
		input:        ti,
	}
	if cfg != nil {
		m.sessionStats = governor.Stats{Model: cfg.Model}
		m.cwd = cfg.Runtime.Workspace
	}
	if ag != nil {
		// Prime the stats from the agent so a resumed session shows its restored
		// cost immediately, and a switched-into agent shows the carried-over run
		// total before the next turn.
		m.sessionStats = ag.SessionStats()
		m.globalStats = ag.GlobalStats()
	}
	if ag != nil {
		// When resuming, the agent is seeded with prior messages; replay them
		// into the chat view so the operator sees the restored conversation. A
		// fresh session holds only the system message, which renders nothing.
		m.prefillFromHistory(ag.History())
	} else {
		// Agent-less: greet with the agent picker so the operator knows to
		// select one with /new.
		m.showAgentPicker()
	}
	return m
}

// SwitchTarget returns the agent folder name the operator asked to switch to
// via /new <agent>, or "" if the program exited without a switch.
func (m *Model) SwitchTarget() string { return m.switchTo }

// SwitchResume returns the session id to resume after a cross-agent switch, or
// "" when the switch starts a fresh session.
func (m *Model) SwitchResume() string { return m.switchResume }

// prefillFromHistory renders a restored conversation into the chat buffer,
// approximating how the live turn loop renders each message kind. The system
// message is not shown. Does nothing when there is no prior conversation.
func (m *Model) prefillFromHistory(history []llm.Message) {
	hasConversation := false
	for _, msg := range history {
		if msg.Role != llm.RoleSystem {
			hasConversation = true
			break
		}
	}
	if !hasConversation {
		return
	}

	m.appendLine(styleMuted.Render("↻ resumed previous session"))
	for _, msg := range history {
		switch msg.Role {
		case llm.RoleUser:
			m.appendLine(styleUser.Render("You: ") + msg.Content)
		case llm.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				m.appendLine(styleAssist.Render(msg.Content))
				m.lastAssistant = msg.Content
			}
			for _, tc := range msg.ToolCalls {
				m.appendLine(styleTool.Render(fmt.Sprintf("→ %s %s", tc.Name, truncate(tc.Arguments, 200))))
			}
		case llm.RoleTool:
			if rest, ok := strings.CutPrefix(msg.Content, "error:"); ok {
				m.appendLine(styleErr.Render("  ✗ " + truncate(strings.TrimSpace(rest), 600)))
			} else {
				m.appendLine(styleToolRes.Render("  ✓ " + truncate(msg.Content, 600)))
			}
		}
	}
}

// Init wires up the event-pump subscription. In the agent-less state there is
// no agent to pump events from, so it does nothing until an agent is selected.
func (m *Model) Init() tea.Cmd {
	if m.ag == nil {
		return nil
	}
	return waitForEvent(m.ag.Events())
}

// waitForEvent returns a tea.Cmd that yields one agent event as a tea.Msg.
// After every event the Update handler re-subscribes by returning waitForEvent
// again — a classic "tick"-style pump.
func waitForEvent(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return agentClosedMsg{}
		}
		return ev
	}
}

type agentClosedMsg struct{}

// Update handles keys, window resizes, and agent events.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.relayout()

	case tea.MouseMsg:
		// Forward all mouse events to the chat viewport. It only acts on
		// wheel events (MouseWheelEnabled defaults to true); clicks and
		// drags are no-ops for us. Track auto-stick after each wheel so
		// scrolling back down re-engages it.
		var cmd tea.Cmd
		m.chat, cmd = m.chat.Update(msg)
		m.userScrolled = !m.chat.AtBottom()
		cmds = append(cmds, cmd)

	case tea.KeyMsg:
		// Approval modal grabs keys first.
		if m.pendingApproval != nil {
			switch msg.String() {
			case "y", "Y":
				m.pendingApproval.Approve()
				m.pendingApproval = nil
			case "n", "N":
				// Deny this call but let the agent keep iterating — the
				// model may try a different tool / approach.
				m.pendingApproval.Deny()
				m.pendingApproval = nil
			case "q", "Q", "esc":
				// Cancel the entire turn — break the model out of whatever
				// loop it's in and return control to the operator.
				m.pendingApproval.Cancel()
				m.pendingApproval = nil
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		case "esc":
			// Stop the running turn. The agent unwinds at its next
			// cancellation point and reports back with a cancelled
			// AssistantDoneMsg, so the marker below is a promise that the
			// turn is stopping, not that it has stopped. Idle, esc does
			// nothing — the approval modal handles its own esc above.
			if m.busy && m.cancelTurn != nil {
				log.Info("operator cancelled turn")
				m.cancelTurn()
				cmds = append(cmds, m.setToast("cancelling turn..."))
			}
			return m, tea.Batch(cmds...)
		case "ctrl+y":
			cmds = append(cmds, m.copyLastReply())
			return m, tea.Batch(cmds...)
		case "ctrl+t":
			cmds = append(cmds, m.copyTranscript())
			return m, tea.Batch(cmds...)
		case "pgup", "pgdown":
			// Selectively forward page-scroll keys to the viewport. The
			// viewport's default keymap also recognises f/b/u/d/j/k/space,
			// but those would steal characters while typing; routing only
			// these explicit ones keeps typing intact.
			var cmd tea.Cmd
			m.chat, cmd = m.chat.Update(msg)
			m.userScrolled = !m.chat.AtBottom()
			return m, tea.Batch(append(cmds, cmd)...)
		case "ctrl+home":
			m.chat.GotoTop()
			m.userScrolled = true
			return m, tea.Batch(cmds...)
		case "ctrl+end":
			m.chat.GotoBottom()
			m.userScrolled = false
			return m, tea.Batch(cmds...)
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text != "" && strings.HasPrefix(text, "/") {
				// Slash command: dispatch to a built-in handler instead of
				// sending the text to the agent.
				name, args := splitCommand(text)
				m.input.SetValue("")
				if cmd, ok := lookupCommand(name); ok {
					// Echo the command into the chat for visibility / history,
					// styled distinctly from regular user messages.
					echo := "» /" + name
					if args != "" {
						echo += " " + args
					}
					m.appendLine(styleMuted.Render(echo))
					return m, cmd.Handler(m, args)
				}
				m.appendLine(styleErr.Render("unknown command: /" + name + " — try /help"))
				return m, tea.Batch(cmds...)
			}
			if text != "" && m.ag == nil {
				// Agent-less: there is nothing to send to. Point the operator
				// at the picker.
				m.appendLine(styleErr.Render("no agent selected — use /new <agent> (see /agents)"))
				m.input.SetValue("")
				return m, tea.Batch(cmds...)
			}
			if text != "" {
				// Sending a message is an explicit "show me what happens
				// next" signal — snap back to the bottom even if the user
				// scrolled up while reading the previous reply. Without
				// this the just-typed message lands in chatBuf below the
				// visible area and the user thinks the UI froze.
				m.userScrolled = false
				m.appendLine(styleUser.Render("You: ") + text)
				m.input.SetValue("")
				// The cmd goroutine runs alongside the bubbletea event loop,
				// so capture only the pointer-stable fields it needs — never
				// m itself, since that would pin the whole TUI model into a
				// background goroutine.
				ag := m.ag
				cmds = append(cmds, m.startTurn(func(turnCtx context.Context) {
					ag.Send(turnCtx, text)
				}))
			}
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
		}

	case agent.AssistantDeltaMsg:
		m.pendingAs.WriteString(msg.Text)
		m.renderPending = true
		if !m.renderTicking {
			m.renderTicking = true
			cmds = append(cmds, renderTick())
		}
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.AssistantMessageMsg:
		m.commitAssistant()
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.AssistantDoneMsg:
		// A turn that ran to completion has already committed its last message
		// via AssistantMessageMsg; this catches the paths that end without one
		// — a cancelled turn holding a half-streamed reply, and /compact.
		m.commitAssistant()
		if msg.Cancelled {
			// Goes after the partial reply so the transcript reads in the
			// order it happened, and stays in the scrollback so a reply that
			// stops mid-sentence is explained later, not just in the moment.
			m.appendLine(styleMuted.Render("⨯ turn cancelled"))
			cmds = append(cmds, m.setToast("turn cancelled"))
		}
		m.busy = false
		m.cancelTurn = nil
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.ToolCallStartMsg:
		m.appendLine(styleTool.Render(fmt.Sprintf("→ %s %s", msg.Name, truncate(msg.Args, 200))))
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.ToolCallResultMsg:
		if msg.Err != "" {
			m.appendLine(styleErr.Render("  ✗ " + msg.Err))
		} else {
			m.appendLine(styleToolRes.Render("  ✓ " + truncate(msg.Result, 600)))
		}
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.TimeoutMsg:
		block := styleWarnBlk.Render(fmt.Sprintf("Command timed out\n%s\n%s", msg.Command, msg.Message))
		m.appendLine(block)
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.StatsUpdateMsg:
		m.sessionStats = msg.Session
		m.globalStats = msg.Global
		m.cwd = msg.CWD
		m.noteContextFill(msg.ContextTokens, msg.ContextBudget)
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.HistoryTrimmedMsg:
		// Stays in the scrollback rather than in a toast: it explains gaps in
		// what the model remembers minutes after it scrolls past.
		m.appendLine(styleMuted.Render(renderHistoryTrimmed(msg)))
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.ApprovalRequestMsg:
		m.pendingApproval = msg.Request
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case agent.ErrorMsg:
		m.appendLine(styleErr.Render("error: " + msg.Err))
		m.busy = false
		cmds = append(cmds, waitForEvent(m.ag.Events()))

	case toastExpiredMsg:
		// Only clear if no fresher toast has replaced this one.
		if msg.version == m.toastVersion {
			m.toast = ""
		}

	case busyTickMsg:
		// Advance the dot animation and re-arm — but only while we're
		// still busy. Once the turn finishes, the chain dies on its own.
		if m.busy {
			m.busyFrame++
			cmds = append(cmds, busyTick())
		}

	case renderTickMsg:
		// Coalesced flush: repaint the chat viewport with whatever streamed
		// deltas have arrived since the last tick. A new delta arriving
		// after this handler runs will re-arm the tick from the delta
		// handler, so the chain self-terminates once streaming stops.
		m.renderTicking = false
		if m.renderPending {
			m.renderPending = false
			m.renderChat()
		}

	case agentClosedMsg:
		return m, tea.Quit
	}

	return m, tea.Batch(cmds...)
}

// View renders the full screen.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting harness-go..."
	}

	// Layout: chat (left, ~75%) | sidebar (right, ~25%), then input full width.
	sidebarW := m.width / 4
	if sidebarW < 28 {
		sidebarW = 28
	}
	chatW := m.width - sidebarW - 4 // border allowance
	if chatW < 20 {
		chatW = 20
	}
	inputH := 3
	// One-row status line between the main area and the input. Always
	// rendered (empty when idle) so layout doesn't shift the chat as the
	// agent transitions in and out of busy.
	statusH := 1
	mainH := m.height - inputH - statusH - 2
	if mainH < 5 {
		mainH = 5
	}

	chatBox := styleChatBorder.
		Width(chatW).
		Height(mainH).
		Render(m.chat.View())

	sidebar := styleSidebarBorder.
		Width(sidebarW).
		Height(mainH).
		Render(m.renderSidebar())

	main := lipgloss.JoinHorizontal(lipgloss.Top, chatBox, sidebar)

	statusLine := m.renderStatus(m.width - 2)

	inputBox := styleInputBorder.
		Width(m.width - 2).
		Render(m.input.View())

	full := lipgloss.JoinVertical(lipgloss.Left, main, statusLine, inputBox)

	if m.pendingApproval != nil {
		modal := styleApproval.Render(m.renderApproval())
		return overlayCenter(full, modal, m.width, m.height)
	}
	return styleApp.Render(full)
}

// relayout adjusts inner viewport/input widths after a WindowSizeMsg. The
// actual wrap-cache rebuild happens inside renderChat when it notices the
// width has changed.
func (m *Model) relayout() {
	sidebarW := m.width / 4
	if sidebarW < 28 {
		sidebarW = 28
	}
	chatW := m.width - sidebarW - 6
	if chatW < 20 {
		chatW = 20
	}
	inputH := 3
	statusH := 1
	m.chat.Width = chatW
	m.chat.Height = m.height - inputH - statusH - 4
	m.input.Width = m.width - 6
	m.renderChat()
}

// renderStatus produces the single-line "status bar" between the chat
// viewport and the input box. While a turn is in flight it shows an
// animated indicator ("● streaming response.", "..", "...", cycling) and the
// key that stops the turn. When idle the line is blank — the row is always
// reserved so layout doesn't shift as the agent transitions in and out of busy.
func (m *Model) renderStatus(width int) string {
	if !m.busy {
		return strings.Repeat(" ", width)
	}
	dots := 1 + m.busyFrame%3
	// The trailing spaces keep the hint at a fixed column while the dots cycle.
	line := "● streaming response" + strings.Repeat(".", dots) + strings.Repeat(" ", 3-dots) + "  [esc] stop"
	// Pad to full width so the underlying screen doesn't keep stale bytes
	// from a previous frame.
	if pad := width - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return styleAccent(line)
}

// copyLastReply copies the most recent completed assistant turn to the
// system clipboard, plain text. Returns a tea.Cmd that schedules clearing
// the toast.
func (m *Model) copyLastReply() tea.Cmd {
	if m.lastAssistant == "" {
		return m.setToast("no assistant reply to copy yet")
	}
	if err := clipboard.WriteAll(m.lastAssistant); err != nil {
		log.Warn("clipboard write failed", "err", err)
		return m.setToast(fmt.Sprintf("clipboard error: %v", err))
	}
	log.Info("copied last reply", "chars", len(m.lastAssistant))
	return m.setToast(fmt.Sprintf("copied last reply (%d chars)", len(m.lastAssistant)))
}

// copyTranscript copies the full chat buffer to the clipboard, with ANSI
// styling stripped so the receiving app gets readable plain text.
func (m *Model) copyTranscript() tea.Cmd {
	body := stripANSI(m.chatBuf.String())
	if body == "" {
		return m.setToast("nothing to copy yet")
	}
	if err := clipboard.WriteAll(body); err != nil {
		log.Warn("clipboard write failed", "err", err)
		return m.setToast(fmt.Sprintf("clipboard error: %v", err))
	}
	log.Info("copied transcript", "chars", len(body))
	return m.setToast(fmt.Sprintf("copied transcript (%d chars)", len(body)))
}

// setToast records a short status hint and returns a tea.Cmd that fires a
// toastExpiredMsg after toastTTL. The version field guards against a stale
// timer clearing a newer toast.
func (m *Model) setToast(s string) tea.Cmd {
	m.toastVersion++
	v := m.toastVersion
	m.toast = s
	return tea.Tick(toastTTL, func(time.Time) tea.Msg {
		return toastExpiredMsg{version: v}
	})
}

// stripANSI removes CSI escape sequences (the ones lipgloss emits for colour
// and text attributes) so copied text is plain.
func stripANSI(s string) string {
	return ansiCSI.ReplaceAllString(s, "")
}

// resetChat clears the chat view back to empty. Used when switching sessions.
func (m *Model) resetChat() {
	m.chatBuf.Reset()
	m.wrappedBody.Reset()
	m.pendingAs.Reset()
	m.renderPending = false
	m.lastAssistant = ""
	m.userScrolled = false
	m.renderChat()
}

// refuseIfBusy reports whether a turn is in flight and, if so, prints a notice
// telling the operator to wait. Session-switching commands use this to avoid
// mutating the conversation while the agent is mid-turn.
func (m *Model) refuseIfBusy(cmd string) bool {
	if m.busy {
		m.appendLine(styleErr.Render(cmd + ": agent is busy — wait for the current turn to finish, or press esc to stop it"))
		return true
	}
	return false
}

// startTurn runs work that talks to the model on a command goroutine, wiring
// the busy indicator and the stop key to it. A user message and /compact both
// go through here: they differ in what they ask the model, not in how the UI
// has to behave while they wait for it.
//
// Do NOT queue a waitForEvent alongside the returned command. The consumer
// chain started in Init() already keeps exactly one reader blocked on the event
// channel at all times, each handler in Update returning the next waitForEvent.
// A second reader would race it: two goroutines blocked on the same channel
// hand their events to bubbletea in goroutine-completion order rather than
// arrival order, and streamed text renders scrambled.
func (m *Model) startTurn(run func(context.Context)) tea.Cmd {
	var cmds []tea.Cmd
	turnCtx, cancelTurn := context.WithCancel(m.ctx)
	if !m.busy {
		m.busy = true
		m.busyFrame = 0
		// Work refused for landing on top of a running turn must not take the
		// stop key with it: esc has to keep pointing at the turn that is
		// actually running.
		m.cancelTurn = cancelTurn
		cmds = append(cmds, busyTick())
	}
	cmds = append(cmds, func() tea.Msg {
		defer cancelTurn()
		run(turnCtx)
		return nil // events arrive via the event channel
	})
	return tea.Batch(cmds...)
}

// requireAgent reports whether an agent is selected and, if not, prints a
// notice. Commands that operate on the current agent/session guard with this.
func (m *Model) requireAgent(cmd string) bool {
	if m.ag == nil {
		m.appendLine(styleErr.Render(cmd + ": no agent selected — use /new <agent> (see /agents)"))
		return false
	}
	return true
}

// renderAgentsList renders the available agents (folders under agentsDir that
// contain an agent.md), marking the active one, followed by a usage hint.
func (m *Model) renderAgentsList() string {
	names, err := config.DiscoverAgents(m.agentsDir)
	if err != nil {
		return styleErr.Render("agents: " + err.Error())
	}
	if len(names) == 0 {
		return styleMuted.Render("no agents found in " + m.agentsDir)
	}
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render("Available agents"))
	for _, n := range names {
		marker := "  "
		if n == m.currentAgent {
			marker = "* "
		}
		fmt.Fprintf(&b, "%s%s\n", marker, n)
	}
	b.WriteString(styleMuted.Render("use /new <agent> to start a session"))
	return b.String()
}

// showAgentPicker greets the operator in the agent-less state with the list of
// available agents and how to start one.
func (m *Model) showAgentPicker() {
	m.appendLine(styleMuted.Render("No agent selected."))
	m.appendLine(m.renderAgentsList())
}

// commitAssistant moves the reply streamed so far out of the pending buffer and
// into the transcript. Until it runs, renderChat draws that text below every
// committed line, so a reply left pending across a tool call would sit under
// the tool traffic it introduced and slide further down with each one.
func (m *Model) commitAssistant() {
	if m.pendingAs.Len() == 0 {
		return
	}
	// Render and clear pendingAs BEFORE appendLine — appendLine calls
	// renderChat synchronously, and renderChat re-adds pendingAs on top of
	// chatBuf. Without the reset-first, the reply shows twice until the next
	// event triggers another render.
	plain := m.pendingAs.String()
	rendered := styleAssist.Render(plain)
	m.pendingAs.Reset()
	m.lastAssistant = plain
	m.appendLine(rendered)
	// appendLine already flushed. Clear the coalescing flag so any
	// still-scheduled renderTickMsg becomes a no-op instead of doing a second
	// identical repaint.
	m.renderPending = false
}

func (m *Model) appendLine(s string) {
	if m.chatBuf.Len() > 0 {
		m.chatBuf.WriteString("\n")
	}
	m.chatBuf.WriteString(s)
	if m.wrapWidth > 0 {
		// Wrap only the new entry and append to the cache — the O(1)-amortised
		// path that keeps streaming cost independent of transcript length.
		// ansi.Wrap treats each embedded \n as a hard break, so wrapping one
		// entry in isolation and joining with \n produces the same visible
		// output as wrapping the concatenation.
		if m.wrappedBody.Len() > 0 {
			m.wrappedBody.WriteString("\n")
		}
		m.wrappedBody.WriteString(ansi.Wrap(s, m.wrapWidth, ""))
	}
	m.renderChat()
}

func (m *Model) renderChat() {
	w := m.chat.Width
	if w <= 0 {
		// Viewport not sized yet (pre-WindowSizeMsg). Push the raw text
		// through so prefilled history isn't lost; the first WindowSizeMsg
		// arrives soon after and re-renders with wrap.
		m.chat.SetContent(m.chatBuf.String())
		return
	}
	if m.wrapWidth != w {
		m.rebuildWrapped(w)
	}
	// Pre-wrap the body before handing it to SetContent. Without this the
	// viewport's View() picks the last Height LOGICAL lines and lipgloss
	// then re-wraps them — if any wrap to multiple visual lines, lipgloss's
	// MaxHeight clips the BOTTOM of the rendered block, hiding the end of
	// the most recent reply. ansi.Wrap is ANSI-aware and width-correct for
	// wide-cell characters.
	body := m.wrappedBody.String()
	if m.pendingAs.Len() > 0 {
		pending := ansi.Wrap(styleAssist.Render(m.pendingAs.String()), w, "")
		if body != "" {
			body = body + "\n" + pending
		} else {
			body = pending
		}
	}
	m.chat.SetContent(body)
	if !m.userScrolled {
		m.chat.GotoBottom()
	}
}

// rebuildWrapped recomputes the wrapped-line cache from chatBuf at the given
// width. Called from renderChat when the viewport width changes (resize, or
// the first WindowSizeMsg after startup). Never called on the per-delta hot
// path — that's the point of the cache.
func (m *Model) rebuildWrapped(width int) {
	m.wrapWidth = width
	m.wrappedBody.Reset()
	if m.chatBuf.Len() == 0 {
		return
	}
	m.wrappedBody.WriteString(ansi.Wrap(m.chatBuf.String(), width, ""))
}

func (m *Model) renderSidebar() string {
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render("Agent"))
	if m.cfg != nil {
		fmt.Fprintln(&b, m.cfg.Name)
	} else {
		fmt.Fprintln(&b, styleMuted.Render("(none — /new <agent>)"))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styleHeader.Render("Model"))
	if m.sessionStats.Model != "" {
		fmt.Fprintln(&b, m.sessionStats.Model)
	} else {
		fmt.Fprintln(&b, styleMuted.Render("—"))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styleHeader.Render("Runtime"))
	if m.runner != nil {
		fmt.Fprintln(&b, "docker")
		fmt.Fprintln(&b, styleMuted.Render(truncate(m.cwd, 26)))
	} else {
		fmt.Fprintln(&b, styleMuted.Render("—"))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styleHeader.Render("Session"))
	fmt.Fprintf(&b, "in:  %d\n", m.sessionStats.TotalInputTokens)
	fmt.Fprintf(&b, "out: %d\n", m.sessionStats.TotalOutputTokens)
	fmt.Fprintf(&b, "tot: %d\n", m.sessionStats.TotalTokens)
	if m.sessionStats.CostAvailable {
		fmt.Fprintf(&b, "$%.4f\n", m.sessionStats.EstimatedCostUSD)
	} else {
		fmt.Fprintln(&b, styleMuted.Render("$ N/A"))
	}
	if m.contextBudget > 0 && m.contextTokens > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, styleHeader.Render("Context"))
		percent := m.contextTokens * 100 / m.contextBudget
		line := fmt.Sprintf("%s / %s  %d%%",
			humanTokens(m.contextTokens), humanTokens(m.contextBudget), percent)
		if percent >= contextWarnPercent {
			fmt.Fprintln(&b, styleAccent(line))
		} else {
			fmt.Fprintln(&b, line)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styleHeader.Render("Run total"))
	fmt.Fprintf(&b, "tok: %d\n", m.globalStats.TotalTokens)
	if m.globalStats.CostAvailable {
		fmt.Fprintf(&b, "$%.4f\n", m.globalStats.EstimatedCostUSD)
	} else {
		fmt.Fprintln(&b, styleMuted.Render("$ N/A"))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styleHeader.Render("Scroll"))
	if m.userScrolled {
		fmt.Fprintln(&b, styleAccent(fmt.Sprintf("↑ %d%% (^end ↓)",
			int(m.chat.ScrollPercent()*100))))
	} else {
		fmt.Fprintln(&b, styleMuted.Render("at bottom"))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, styleHeader.Render("Keys"))
	if m.toast != "" {
		fmt.Fprintln(&b, styleOK.Render(truncate(m.toast, 26)))
	}
	fmt.Fprintln(&b, styleMuted.Render("^y copy reply"))
	fmt.Fprintln(&b, styleMuted.Render("^t copy chat"))
	fmt.Fprintln(&b, styleMuted.Render("pgup/pgdn scroll"))
	fmt.Fprintln(&b, styleMuted.Render("^home/^end top/btm"))
	fmt.Fprintln(&b, styleMuted.Render("wheel  scroll"))
	fmt.Fprintln(&b, styleMuted.Render("/exit  quit (or /help)"))
	fmt.Fprintln(&b, styleMuted.Render("^c quit"))
	return b.String()
}

func (m *Model) renderApproval() string {
	r := m.pendingApproval
	return fmt.Sprintf(
		"%s\n\nTool: %s\n\nArgs:\n%s\n\n%s",
		styleHeader.Render("Approval required"),
		styleAccent(r.ToolName),
		r.Args,
		styleMuted.Render("[y] approve   [n] deny   [q/esc] cancel turn"),
	)
}

// contextWarnPercent is the fill level at which the context stops being
// background information — far enough ahead of the ceiling that /compact is
// still a choice rather than a rescue.
const contextWarnPercent = 85

// noteContextFill records what the last request cost and, the first time it
// crosses the warning mark, says so in the conversation. The sidebar figure is
// there to be watched; this is for the operator who is reading the transcript
// rather than the gauge, and who otherwise meets the ceiling as a failed turn.
//
// It arms again once the fill drops back, so the warning follows a /compact or
// a window advance that fills up again instead of being spent once a session.
func (m *Model) noteContextFill(tokens, budget int) {
	m.contextTokens = tokens
	m.contextBudget = budget
	if budget <= 0 || tokens <= 0 {
		return
	}
	percent := tokens * 100 / budget
	if percent < contextWarnPercent {
		m.contextWarned = false
		return
	}
	if m.contextWarned {
		return
	}
	m.contextWarned = true
	m.appendLine(styleWarn.Render(fmt.Sprintf(
		"⚠ context %d%% full (%s of %s tokens) — /compact to keep the earlier conversation as a summary before it is dropped",
		percent, humanTokens(tokens), humanTokens(budget))))
}

// renderHistoryTrimmed describes what left the conversation, in the terms that
// matter to the operator: how much they lost, and whether they can get the gist
// of it back.
func renderHistoryTrimmed(msg agent.HistoryTrimmedMsg) string {
	if msg.Summarised {
		return fmt.Sprintf("⋯ compacted %d earlier messages into a summary (%s → %s tokens)",
			msg.DroppedMessages, humanTokens(msg.BeforeTokens), humanTokens(msg.AfterTokens))
	}

	var did []string
	if msg.DroppedMessages > 0 {
		did = append(did, fmt.Sprintf("dropped %d earlier messages", msg.DroppedMessages))
	}
	if msg.ShrunkResults > 0 {
		did = append(did, fmt.Sprintf("shortened %d tool results", msg.ShrunkResults))
	}
	return fmt.Sprintf("⋯ %s to stay within the context budget (%s → %s tokens)",
		strings.Join(did, " and "), humanTokens(msg.BeforeTokens), humanTokens(msg.AfterTokens))
}

// humanTokens renders a token count for a sidebar column 26 characters wide.
func humanTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.0fk", float64(n)/1000)
}

func styleAccent(s string) string {
	return lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(s)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// overlayCenter draws overlay centered on top of base. BubbleTea has no real
// compositing, so we render base, then splice the modal lines into the middle.
func overlayCenter(base, overlay string, w, h int) string {
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")
	ow := lipgloss.Width(overlay)
	oh := len(overlayLines)
	top := (h - oh) / 2
	if top < 0 {
		top = 0
	}
	left := (w - ow) / 2
	if left < 0 {
		left = 0
	}
	for i, ol := range overlayLines {
		y := top + i
		if y >= len(baseLines) {
			break
		}
		// Pad/truncate the base line to width w, then splice in the overlay line.
		baseLine := baseLines[y]
		// strip ANSI for width math is non-trivial; we just append on the right
		// when the line is shorter than left. This is a best-effort overlay.
		if lipgloss.Width(baseLine) < left {
			baseLines[y] = baseLine + strings.Repeat(" ", left-lipgloss.Width(baseLine)) + ol
		} else {
			baseLines[y] = ol
		}
	}
	return strings.Join(baseLines, "\n")
}
