// Root BubbleTea model: chat viewport on the left, stats sidebar on the right,
// input bar across the bottom, and an approval panel docked above the input
// when a restricted tool needs operator confirmation.
package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/ruvolof/yaaih/internal/agent"
	"github.com/ruvolof/yaaih/internal/config"
	"github.com/ruvolof/yaaih/internal/executor"
	"github.com/ruvolof/yaaih/internal/governor"
	"github.com/ruvolof/yaaih/internal/llm"
	"github.com/ruvolof/yaaih/internal/logging"
	"github.com/ruvolof/yaaih/internal/session"
)

var log = logging.For("tui")

// ansiCSI matches CSI sequences (everything lipgloss emits for colour, bold,
// reset, etc.). Used to strip styling when copying text to the clipboard so
// the receiving app gets plain text.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// writeClipboard puts text on the system clipboard. It writes straight to the
// terminal rather than through the program, in a single write so it cannot
// split a frame the renderer is flushing.
var writeClipboard = func(text string) error {
	_, err := io.WriteString(os.Stdout, osc52(text))
	return err
}

// osc52 is the escape sequence asking the terminal to set the system
// clipboard. The terminal owns the clipboard, so copying needs no helper
// program and works over SSH; a terminal without OSC 52 support, or tmux
// without set-clipboard on, drops the sequence without a word.
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}

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
	events *eventRelay // nil when there is no agent
	// terminal is the operator's shell in the sandbox; nil without one.
	terminal *operatorTerminal

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

	chat       viewport.Model
	transcript transcript
	pendingAs  strings.Builder // assistant content currently being streamed

	// renderPending / renderTicking coalesce bursts of AssistantDeltaMsg into
	// one repaint every renderTickInterval instead of one per chunk. Cleared
	// when a tick fires or when commitAssistant forces a final flush.
	renderPending bool
	renderTicking bool

	// replyRenderTicking is true while a replyRenderTickMsg is scheduled.
	replyRenderTicking bool

	// userScrolled tracks whether the user has manually scrolled the chat
	// viewport away from the bottom. When true, new content (streaming
	// deltas, tool results, etc.) is appended without snapping the view
	// back to the bottom — they get to read history undisturbed. Reset to
	// false when the viewport returns to bottom (Ctrl+End, PgDn to bottom,
	// or wheel-down to bottom) or when the user sends a new message.
	userScrolled bool

	// selection is the text the operator has dragged over, or nil. dragging
	// is true from the press that starts a drag to its release; a chat view
	// does not follow new content in between, so the text does not slide
	// under the pointer.
	selection *textSelection
	dragging  bool

	// draggingThumb is true from a press on the chat's scrollbar to its
	// release; thumbGrab is the row of the thumb the pointer holds it by.
	draggingThumb bool
	thumbGrab     int

	// busy is true while a turn is in flight (between the user pressing
	// Enter and the AssistantDoneMsg that ends it). busyFrame advances every
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
	// assistant message so ctrl+y can copy it without depending on the
	// transcript's styling. Updated in commitAssistant.
	lastAssistant string

	// toast is a short status hint shown in the sidebar (e.g. "copied 412
	// chars"). Cleared by a tea.Tick scheduled with toastTTL.
	toast        string
	toastVersion int // monotonic; lets stale Ticks find their toast already replaced

	input        inputBox
	inputHistory *InputHistory

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

	// spendWarned keeps the approaching-the-limit warning to one per session;
	// costLimitWarned the one about a cost limit the provider cannot enforce.
	spendWarned     bool
	costLimitWarned bool

	// pendingApproval is the restricted call waiting on the operator. Its panel
	// takes approvalHeight rows above the input, measured on every relayout,
	// and the conversation shrinks to make room for it rather than being
	// covered.
	pendingApproval   *agent.ApprovalRequest
	approvalFields    []approvalField
	approvalScroll    int
	approvalCollapsed bool
	approvalHeight    int

	// sidebarScroll is how many sidebar rows the wheel has scrolled past, for
	// a terminal too short to show the whole sidebar.
	sidebarScroll int
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

// A resize draws the replies on screen as Markdown straight away and leaves
// the rest as stand-ins; replyRenderTickMsg draws those in batches of about
// replyRenderBudget, one every replyRenderInterval, so a long session stays
// responsive to keys and further resizes while it catches up.
const (
	replyRenderInterval = 16 * time.Millisecond
	replyRenderBudget   = 8 * time.Millisecond
)

type replyRenderTickMsg struct{}

func replyRenderTick() tea.Cmd {
	return tea.Tick(replyRenderInterval, func(time.Time) tea.Msg { return replyRenderTickMsg{} })
}

// New constructs the TUI model. Pass the cancellable context the program is
// using; agent.Send is invoked off the BubbleTea loop with the same context.
// inputHistory is shared by every Model of the run, so ↑ reaches inputs sent
// to agents used before a switch; nil starts an empty one.
//
// Returns a *Model so the same allocation lives across every Update/View
// pass. Returning Model by value would cause BubbleTea to copy the struct
// each iteration, and the strings.Builder fields cache a self-pointer via
// noescape(unsafe.Pointer); a stale copy of that pointer is what triggered a
// "found bad pointer in Go heap" GC crash on the first chat message.
func New(ctx context.Context, cfg *config.Config, ag *agent.Agent, runner executor.Executor, agentsDir, currentAgent string, inputHistory *InputHistory) *Model {
	if inputHistory == nil {
		inputHistory = NewInputHistory()
	}

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
		input:        newInputBox(80),
		inputHistory: inputHistory,
	}
	if cfg != nil {
		m.sessionStats = governor.Stats{Model: cfg.Model}
		m.cwd = cfg.Runtime.Workspace
	}
	if ag != nil {
		m.events = newEventRelay(ag.Events())
		if shells, ok := runner.(executor.OperatorShells); ok {
			m.terminal = newOperatorTerminal(ctx, shells, m.events)
		}
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
// approximating how the live turn loop renders each message kind. System
// messages are not shown, except the briefing a spawned session opened with.
// Does nothing when there is no prior conversation.
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
		case llm.RoleSystem:
			if briefing, ok := agent.SpawnBriefing(msg); ok {
				var parent, child session.ID
				if m.ag != nil {
					parent, child = m.ag.SessionParent(), m.ag.CurrentSession()
				}
				m.showSpawnedSession(parent, child, briefing)
			}
		case llm.RoleUser:
			m.appendLine(styleUser.Render("You: ") + msg.Content)
		case llm.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				m.appendReply(msg.Content)
				m.lastAssistant = msg.Content
			}
			for _, tc := range msg.ToolCalls {
				m.startTool(tc.ID, tc.Name, tc.Arguments)
			}
		case llm.RoleTool:
			// A session file keeps the reply but not whether the tool failed;
			// every failure reply the agent records starts with this prefix.
			m.finishTool(msg.ToolCallID, msg.Name, msg.Content, strings.HasPrefix(msg.Content, "error:"))
		}
	}
}

// Init wires up the event-pump subscription. In the agent-less state there is
// no agent to pump events from, so it does nothing until an agent is selected.
func (m *Model) Init() tea.Cmd {
	return m.nextEvent()
}

// nextEvent returns a tea.Cmd that yields one agent event as a tea.Msg.
// After every event the Update handler re-subscribes by returning nextEvent
// again — a classic "tick"-style pump.
func (m *Model) nextEvent() tea.Cmd {
	if m.events == nil {
		return nil
	}
	events := m.events.out
	return func() tea.Msg {
		ev, ok := <-events
		if !ok {
			return agentClosedMsg{}
		}
		return ev
	}
}

type agentClosedMsg struct{}

// terminalClosedMsg reports the end of a visit to the operator's shell.
type terminalClosedMsg struct{ err error }

// Update handles keys, window resizes, and agent events.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.relayout()
		cmds = append(cmds, m.scheduleReplyRendering())

	case tea.MouseMsg:
		if handled, cmd := m.dragSelection(msg); handled {
			return m, cmd
		}
		if m.dragScrollbar(msg) {
			return m, nil
		}
		if m.pendingApproval != nil && m.handleApprovalMouse(msg) {
			return m, nil
		}
		if m.overSidebar(msg.X, msg.Y) {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.scrollSidebar(-3)
			case tea.MouseButtonWheelDown:
				m.scrollSidebar(3)
			}
			return m, nil
		}
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if !m.grabScrollbar(msg.X, msg.Y) {
				m.startSelection(msg.X, msg.Y)
			}
			return m, nil
		}
		// Forward the rest to the chat viewport, which only acts on the wheel.
		// Track auto-stick after each wheel so scrolling back down re-engages
		// it.
		var cmd tea.Cmd
		m.chat, cmd = m.chat.Update(msg)
		m.userScrolled = !m.chat.AtBottom() || m.dragging
		cmds = append(cmds, cmd)

	case tea.KeyMsg:
		if m.pendingApproval != nil {
			if m.handleApprovalKey(msg.String()) || !keysBesideApproval[msg.String()] {
				return m, nil
			}
		}
		if !keysKeepingSelection[msg.String()] {
			m.selection = nil
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, m.quit()
		case "ctrl+]":
			if m.terminal == nil {
				return m, m.setToast("no sandbox to open a shell in")
			}
			visit := m.terminal.visit(m.pendingApproval != nil)
			return m, tea.Exec(visit, func(err error) tea.Msg { return terminalClosedMsg{err: err} })
		case "esc":
			// Stop the running turn. The agent unwinds at its next
			// cancellation point and reports back with a cancelled
			// AssistantDoneMsg, so the marker below is a promise that the
			// turn is stopping, not that it has stopped. Idle, esc does
			// nothing — the approval panel handles its own esc above.
			if m.busy && m.cancelTurn != nil {
				log.Info("operator cancelled turn")
				m.cancelTurn()
				cmds = append(cmds, m.setToast("cancelling turn..."))
			}
			return m, tea.Batch(cmds...)
		case "ctrl+y":
			if m.selection != nil && m.selection.moved {
				cmds = append(cmds, m.copySelection())
			} else {
				cmds = append(cmds, m.copyLastReply())
			}
			return m, tea.Batch(cmds...)
		case "ctrl+t":
			cmds = append(cmds, m.copyTranscript())
			return m, tea.Batch(cmds...)
		case "ctrl+o":
			target := m.transcript.selected
			if target == nil {
				target = m.transcript.toolBefore(len(m.transcript.entries))
			}
			if target == nil {
				return m, m.setToast("no tool calls to expand")
			}
			m.toggleTool(target)
			return m, tea.Batch(cmds...)
		case "alt+up":
			from := len(m.transcript.entries)
			if m.transcript.selected != nil {
				from = m.transcript.selected.index
			}
			if previous := m.transcript.toolBefore(from); previous != nil {
				m.selectTool(previous)
			}
			return m, tea.Batch(cmds...)
		case "alt+down":
			if m.transcript.selected != nil {
				// Moving down past the newest call leaves selection mode, and
				// ctrl+o goes back to acting on the newest call.
				m.selectTool(m.transcript.toolAfter(m.transcript.selected.index))
			}
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
		case "tab":
			m.completeInputOnTab()
			return m, tea.Batch(cmds...)
		case "up", "down":
			if m.recallInput(msg.String()) {
				return m, tea.Batch(cmds...)
			}
			cmds = append(cmds, m.updateInput(msg))
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" && m.transcript.selected != nil {
				m.toggleTool(m.transcript.selected)
				return m, tea.Batch(cmds...)
			}
			m.inputHistory.record(text)
			if text != "" && strings.HasPrefix(text, "/") {
				// Slash command: dispatch to a built-in handler instead of
				// sending the text to the agent.
				name, args := splitCommand(text)
				m.setInput("")
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
				m.setInput("")
				return m, tea.Batch(cmds...)
			}
			if text != "" {
				// Sending a message is an explicit "show me what happens
				// next" signal — snap back to the bottom even if the user
				// scrolled up while reading the previous reply. Without
				// this the just-typed message lands below the visible area
				// and the user thinks the UI froze. A selection left over
				// from reviewing the last turn would otherwise keep ctrl+o
				// pointed at an old call.
				m.userScrolled = false
				m.selectTool(nil)
				m.appendLine(styleUser.Render("You: ") + text)
				m.setInput("")
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
			cmds = append(cmds, m.updateInput(msg))
		}

	case agent.AssistantDeltaMsg:
		m.pendingAs.WriteString(msg.Text)
		m.renderPending = true
		if !m.renderTicking {
			m.renderTicking = true
			cmds = append(cmds, renderTick())
		}
		cmds = append(cmds, m.nextEvent())

	case agent.AssistantMessageMsg:
		m.commitAssistant()
		cmds = append(cmds, m.nextEvent())

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
		cmds = append(cmds, m.nextEvent())

	case agent.ToolCallStartMsg:
		m.startTool(msg.ID, msg.Name, msg.Args)
		cmds = append(cmds, m.nextEvent())

	case agent.ToolCallResultMsg:
		m.finishTool(msg.ID, msg.Name, msg.Result, msg.Err != "")
		cmds = append(cmds, m.nextEvent())

	case agent.StatsUpdateMsg:
		m.sessionStats = msg.Session
		m.globalStats = msg.Global
		m.cwd = msg.CWD
		m.noteContextFill(msg.ContextTokens, msg.ContextBudget)
		m.noteSpend(msg.Session)
		cmds = append(cmds, m.nextEvent())

	case agent.HistoryTrimmedMsg:
		// Stays in the scrollback rather than in a toast: it explains gaps in
		// what the model remembers minutes after it scrolls past.
		m.appendLine(styleMuted.Render(renderHistoryTrimmed(msg)))
		cmds = append(cmds, m.nextEvent())

	case agent.SessionSpawnedMsg:
		// The briefing streamed into the old session's chat; the new session's
		// chat opens with it instead. The turn on the task is still running.
		m.resetChat()
		m.showSpawnedSession(msg.Parent, msg.Child, msg.Briefing)
		m.appendLine(styleUser.Render("You: ") + msg.Task)
		m.sessionStats = m.ag.SessionStats()
		m.globalStats = m.ag.GlobalStats()
		cmds = append(cmds, m.nextEvent())

	case agent.ApprovalRequestMsg:
		m.openApproval(msg.Request)
		cmds = append(cmds, m.nextEvent())

	case terminalClosedMsg:
		if msg.err != nil {
			m.appendLine(styleErr.Render("operator shell: " + msg.err.Error()))
		}

	case agent.ErrorMsg:
		// Reporting only. What ends a turn — and with it the busy indicator and
		// the stop key — is AssistantDoneMsg, which the agent emits on every
		// path out including the ones that failed. An error that arrives while
		// a turn is running (a rejected second message) must leave the running
		// turn's UI alone.
		m.appendLine(styleErr.Render("error: " + msg.Err))
		cmds = append(cmds, m.nextEvent())

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

	case replyRenderTickMsg:
		m.replyRenderTicking = false
		reading, offset := m.readingPosition()
		m.transcript.renderStaleNear(m.viewTop(reading, offset)+m.chat.Height/2, replyRenderBudget)
		m.renderChatKeeping(reading, offset)
		cmds = append(cmds, m.scheduleReplyRendering())

	case agentClosedMsg:
		return m, tea.Quit
	}

	return m, tea.Batch(cmds...)
}

// View renders the full screen.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting yaaih..."
	}

	// Layout: chat (left, ~75%) | sidebar (right, ~25%), then input full width.
	sidebarW := m.sidebarWidth()
	mainH := m.mainHeight()

	chatBox := m.drawChatScrollbar(styleChatBorder.
		Width(m.chatBoxWidth()).
		Height(mainH).
		Render(m.chatView()))

	sidebar := styleSidebarBorder.
		Width(sidebarW).
		Height(mainH).
		Render(m.sidebarWindow(sidebarW-styleSidebarBorder.GetHorizontalPadding(), mainH))

	main := lipgloss.JoinHorizontal(lipgloss.Top, chatBox, sidebar)

	statusLine := m.renderStatus(m.width - 2)

	inputBox := styleInputBorder.
		Width(m.width - 2).
		Render(m.inputView())

	rows := []string{main, statusLine}
	if m.pendingApproval != nil {
		rows = append(rows, m.renderApprovalPanel())
	}
	rows = append(rows, inputBox)
	return styleApp.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m *Model) sidebarWidth() int {
	return max(m.width/4, 28)
}

// chatBoxWidth is the chat box's width inside its border.
func (m *Model) chatBoxWidth() int {
	return max(m.width-m.sidebarWidth()-4, 20)
}

// mainHeight is the rows inside the border of the chat box and the sidebar:
// the screen less the input box, the approval panel, and the one-row status
// line, which is always reserved (blank when idle) so the chat does not shift
// as the agent goes in and out of busy.
func (m *Model) mainHeight() int {
	return max(m.height-(m.input.Height()+2)-1-m.approvalHeight-2, 5)
}

func (m *Model) sidebarLines() []string {
	return strings.Split(strings.TrimRight(m.renderSidebar(), "\n"), "\n")
}

// sidebarWindow is the part of the sidebar that fits in height rows, from
// sidebarScroll on. A sidebar taller than that would stretch the row and push
// the approval panel and the input off the screen, and a line wider than width
// would wrap onto a row of its own, so both are cut. The first and last rows
// say how much is cut off above and below.
func (m *Model) sidebarWindow(width, height int) string {
	lines := m.sidebarLines()
	first := min(m.sidebarScroll, max(len(lines)-height, 0))
	last := min(first+height, len(lines))
	visible := make([]string, 0, last-first)
	for _, line := range lines[first:last] {
		visible = append(visible, ansi.Truncate(line, width, "…"))
	}
	if first > 0 {
		visible[0] = styleMuted.Render(fmt.Sprintf("▲ %d more", first+1))
	}
	if last < len(lines) {
		visible[len(visible)-1] = styleMuted.Render(fmt.Sprintf("▼ %d more (wheel)", len(lines)-last+1))
	}
	return strings.Join(visible, "\n")
}

func (m *Model) scrollSidebar(delta int) {
	maxScroll := max(len(m.sidebarLines())-m.mainHeight(), 0)
	m.sidebarScroll = min(max(m.sidebarScroll+delta, 0), maxScroll)
}

// overSidebar reports whether screen cell (x, y) is on the sidebar box,
// border included.
func (m *Model) overSidebar(x, y int) bool {
	sidebarLeft := m.chatBoxWidth() + styleChatBorder.GetHorizontalBorderSize()
	return x >= sidebarLeft && y < m.mainHeight()+styleSidebarBorder.GetVerticalBorderSize()
}

// relayout adjusts inner viewport/input sizes after a WindowSizeMsg or a change
// in the input box's height. The actual wrap-cache rebuild happens inside
// renderChat when it notices the width has changed.
func (m *Model) relayout() {
	m.input.SetWidth(m.width - 6)
	// The chat drops a selection itself when its text moves; the approval
	// panel and the input box are laid out again from scratch.
	if m.selection != nil && m.selection.pane != chatPane {
		m.selection = nil
		m.dragging = false
	}
	m.approvalHeight = 0
	if m.pendingApproval != nil {
		m.approvalHeight = lipgloss.Height(m.renderApprovalPanel())
	}
	m.chat.Width = m.chatBoxWidth() - styleChatBorder.GetHorizontalPadding()
	m.chat.Height = m.mainHeight() - 2
	m.renderChat()
}

// relayoutIfSized relayouts once the first WindowSizeMsg has given the screen
// a size; before that there is nothing to lay out.
func (m *Model) relayoutIfSized() {
	if m.width > 0 {
		m.relayout()
	}
}

// renderStatus produces the single-line "status bar" under the chat
// viewport. While a turn is in flight it shows an
// animated indicator ("● streaming response.", "..", "...", cycling) and the
// key that stops the turn, or, while a call waits on the approval panel, that
// the turn is waiting on the operator rather than on the model. When idle the
// line is blank — the row is always reserved so layout doesn't shift as the
// agent transitions in and out of busy.
func (m *Model) renderStatus(width int) string {
	if m.pendingApproval != nil {
		line := "● waiting for your approval of the call below"
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		return styleWarn.Render(line)
	}
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
// system clipboard, as the Markdown the model wrote. Returns a tea.Cmd that
// schedules clearing the toast.
func (m *Model) copyLastReply() tea.Cmd {
	if m.lastAssistant == "" {
		return m.setToast("no assistant reply to copy yet")
	}
	return m.copyToClipboard("last reply", m.lastAssistant)
}

// copyTranscript copies the whole conversation to the clipboard as plain text,
// every tool call in full whether or not it is expanded on screen.
func (m *Model) copyTranscript() tea.Cmd {
	body := m.transcript.plainText()
	if body == "" {
		return m.setToast("nothing to copy yet")
	}
	return m.copyToClipboard("transcript", body)
}

func (m *Model) copyToClipboard(what, text string) tea.Cmd {
	if err := writeClipboard(text); err != nil {
		log.Warn("clipboard write failed", "err", err)
		return m.setToast(fmt.Sprintf("clipboard error: %v", err))
	}
	chars := utf8.RuneCountInString(text)
	log.Info("copied "+what, "chars", chars)
	return m.setToast(fmt.Sprintf("copied %s (%d chars)", what, chars))
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
	m.transcript.reset()
	m.pendingAs.Reset()
	m.renderPending = false
	m.lastAssistant = ""
	m.userScrolled = false
	m.renderChat()
}

// quit leaves the harness, stopping a turn that is still running on the way
// out. Teardown waits for the turn to unwind before it closes the session file
// and the sandbox, so a turn left running is a turn the operator waits for.
func (m *Model) quit() tea.Cmd {
	if m.busy && m.cancelTurn != nil {
		log.Info("quitting with a turn in flight, cancelling it")
		m.cancelTurn()
	}
	return tea.Quit
}

// updateInput passes a key to the input box, making room for it in the layout
// when what was typed changed its height.
func (m *Model) updateInput(msg tea.Msg) tea.Cmd {
	cmd, resized := m.input.Update(msg)
	if resized && m.width > 0 {
		m.relayout()
	}
	return cmd
}

// setInput replaces what is in the input box.
func (m *Model) setInput(text string) {
	if m.input.SetValue(text) && m.width > 0 {
		m.relayout()
	}
}

// recallInput walks the input history for ↑ ("up") or ↓ ("down"), reporting
// whether it did. It does not when the input holds text of the operator's own,
// and the key then moves the cursor instead.
func (m *Model) recallInput(direction string) bool {
	walk := m.inputHistory.previous
	if direction == "down" {
		walk = m.inputHistory.next
	}
	entry, ok := walk(m.input.Value())
	if ok {
		m.setInput(entry)
	}
	return ok
}

// completeInputOnTab completes the slash command in the input box, listing the
// candidates in the conversation when there is no single way to go on.
func (m *Model) completeInputOnTab() {
	current := m.input.Value()
	completed, candidates := m.completeInput(current)
	if completed != current {
		m.setInput(completed)
	}
	if len(candidates) > 0 {
		m.appendLine(renderCompletions(candidates))
	}
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
// Do NOT queue a nextEvent alongside the returned command. The consumer
// chain started in Init() already keeps exactly one reader blocked on the event
// channel at all times, each handler in Update returning the next nextEvent.
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
// into the transcript, where it is drawn as Markdown. Until it runs, renderChat
// draws that text raw below every committed line, so a reply left pending
// across a tool call would sit under the tool traffic it introduced and slide
// further down with each one.
func (m *Model) commitAssistant() {
	if m.pendingAs.Len() == 0 {
		return
	}
	// Clear pendingAs BEFORE appendReply — appendReply calls renderChat
	// synchronously, and renderChat draws pendingAs below the transcript.
	// Without the reset-first, the reply shows twice until the next event
	// triggers another render.
	plain := m.pendingAs.String()
	m.pendingAs.Reset()
	m.lastAssistant = plain
	m.appendReply(plain)
	// appendReply already flushed. Clear the coalescing flag so any
	// still-scheduled renderTickMsg becomes a no-op instead of doing a second
	// identical repaint.
	m.renderPending = false
}

func (m *Model) appendLine(s string) {
	m.transcript.appendText(s)
	m.renderChat()
}

func (m *Model) appendReply(markdown string) {
	m.transcript.appendReply(markdown)
	m.renderChat()
}

// startTool shows a call the agent is about to run, collapsed.
func (m *Model) startTool(id, name, args string) {
	m.transcript.appendTool(&toolCall{id: id, name: name, args: args})
	m.renderChat()
}

// finishTool fills in the reply under the call it answers. A reply whose call
// was never shown gets an entry of its own rather than being dropped.
func (m *Model) finishTool(id, name, result string, failed bool) {
	entry := m.transcript.unansweredTool(id)
	if entry == nil {
		entry = m.transcript.appendTool(&toolCall{id: id, name: name})
	}
	entry.tool.result = result
	entry.tool.answered = true
	entry.tool.failed = failed
	m.transcript.refresh(entry)
	m.renderChat()
}

// toggleTool expands or collapses a tool call, keeping the view where the
// operator is reading instead of jumping to the bottom.
func (m *Model) toggleTool(entry *chatEntry) {
	entry.expanded = !entry.expanded
	m.transcript.refresh(entry)
	m.renderChatInPlace()
	m.scrollToEntry(entry)
}

// selectTool moves the alt+↑/↓ selection to entry, or clears it for nil.
func (m *Model) selectTool(entry *chatEntry) {
	previous := m.transcript.selected
	if previous == entry {
		return
	}
	m.transcript.selected = entry
	m.transcript.refresh(previous, entry)
	m.renderChatInPlace()
	if entry != nil {
		m.scrollToEntry(entry)
	}
}

// renderChatInPlace re-renders without following new content to the bottom.
func (m *Model) renderChatInPlace() {
	m.userScrolled = true
	m.renderChat()
	m.chat.SetYOffset(m.chat.YOffset)
	m.userScrolled = !m.chat.AtBottom()
}

// scrollToEntry brings as much of entry into view as fits, its first line
// always among it.
func (m *Model) scrollToEntry(entry *chatEntry) {
	if m.chat.Height <= 0 {
		return
	}
	top := entry.firstLine
	bottom := top + entry.lineCount - 1
	switch {
	case top < m.chat.YOffset:
		m.chat.SetYOffset(top)
	case bottom >= m.chat.YOffset+m.chat.Height:
		m.chat.SetYOffset(min(top, bottom-m.chat.Height+1))
	}
	m.userScrolled = !m.chat.AtBottom()
}

// chatContentOrigin is the screen cell the chat's first visible line starts
// at. The chat box is the first thing in the layout, so its content starts
// just inside its border and padding.
func chatContentOrigin() (left, top int) {
	return styleChatBorder.GetBorderLeftSize() + styleChatBorder.GetPaddingLeft(),
		styleChatBorder.GetBorderTopSize() + styleChatBorder.GetPaddingTop()
}

// overChatText reports whether screen cell (x, y) is inside the chat viewport.
func (m *Model) overChatText(x, y int) bool {
	left, top := chatContentOrigin()
	return x >= left && x < left+m.chat.Width && y >= top && y < top+m.chat.Height
}

// toolAt returns the tool call drawn at screen cell (x, y), or nil.
func (m *Model) toolAt(x, y int) *chatEntry {
	if !m.overChatText(x, y) {
		return nil
	}
	_, top := chatContentOrigin()
	entry := m.transcript.entryAtLine(y - top + m.chat.YOffset)
	if entry == nil || entry.tool == nil {
		return nil
	}
	return entry
}

func (m *Model) renderChat() {
	m.renderChatKeeping(m.readingPosition())
}

// renderChatKeeping re-renders with reading, offset lines in, kept at the top
// of the view if the operator has scrolled away from the bottom — so a resize,
// or replies above the view being drawn again, does not move what they are
// reading.
func (m *Model) renderChatKeeping(reading *chatEntry, offset int) {
	width := m.chat.Width
	if width <= 0 {
		// Not sized yet; the first WindowSizeMsg lays the transcript out.
		return
	}
	m.transcript.setWidth(width)
	m.renderRepliesInView(reading, offset)
	// Pre-wrap the body before handing it to SetContent. Without this the
	// viewport's View() picks the last Height LOGICAL lines and lipgloss
	// then re-wraps them — if any wrap to multiple visual lines, lipgloss's
	// MaxHeight clips the BOTTOM of the rendered block, hiding the end of
	// the most recent reply. ansi.Wrap is ANSI-aware and width-correct for
	// wide-cell characters.
	body := m.transcript.body.String()
	if m.pendingAs.Len() > 0 {
		pending := m.pendingLayout(width)
		if body != "" {
			body = body + "\n" + pending
		} else {
			body = pending
		}
	}
	m.chat.SetContent(body)
	switch {
	case !m.userScrolled:
		m.chat.GotoBottom()
	case reading != nil:
		m.chat.SetYOffset(m.viewTop(reading, offset))
	}
	if line, changed := m.transcript.takeChange(); changed && m.selection != nil &&
		m.selection.pane == chatPane && m.selection.reaches(line) {
		m.selection = nil
	}
}

// readingPosition is the entry at the top of the view and how many of its
// lines are scrolled past, or nil while the view follows the bottom.
func (m *Model) readingPosition() (*chatEntry, int) {
	if !m.userScrolled {
		return nil, 0
	}
	entry := m.transcript.entryAtLine(m.chat.YOffset)
	if entry == nil {
		return nil, 0
	}
	return entry, m.chat.YOffset - entry.firstLine
}

// viewTop is the transcript line the view starts at, by the current layout.
func (m *Model) viewTop(reading *chatEntry, offset int) int {
	switch {
	case !m.userScrolled:
		return max(m.transcript.lineCount-m.chat.Height, 0)
	case reading != nil:
		return reading.firstLine + min(offset, reading.lineCount-1)
	default:
		return m.chat.YOffset
	}
}

// renderRepliesInView draws the stand-ins in view as Markdown, so the screen
// never shows one. Drawing a reply changes its height and so what is in view;
// each pass draws at least one, so the loop ends.
func (m *Model) renderRepliesInView(reading *chatEntry, offset int) {
	for {
		top := m.viewTop(reading, offset)
		if !m.transcript.renderReplies(top, top+m.chat.Height) {
			return
		}
	}
}

func (m *Model) scheduleReplyRendering() tea.Cmd {
	if m.replyRenderTicking || !m.transcript.hasStandIns() {
		return nil
	}
	m.replyRenderTicking = true
	return replyRenderTick()
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
		if running := runningJobs(m.runner); running > 0 {
			fmt.Fprintln(&b, styleAccent(fmt.Sprintf("jobs running: %d", running)))
		}
	} else {
		fmt.Fprintln(&b, styleMuted.Render("—"))
	}
	fmt.Fprintln(&b)
	if m.ag != nil {
		fmt.Fprint(&b, renderApprovalStanding(m.ag.ApprovalStanding()))
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, styleHeader.Render("Session"))
	if m.ag != nil {
		if parent := m.ag.SessionParent(); parent != "" {
			fmt.Fprintln(&b, styleMuted.Render(truncate("from "+string(parent), 26)))
		}
	}
	fmt.Fprintf(&b, "in:  %d\n", m.sessionStats.TotalInputTokens)
	fmt.Fprintf(&b, "out: %d\n", m.sessionStats.TotalOutputTokens)
	fmt.Fprint(&b, renderSessionSpend(m.sessionStats))
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
	fmt.Fprintln(&b, styleHeader.Render("Keys"))
	if m.toast != "" {
		fmt.Fprintln(&b, styleOK.Render(truncate(m.toast, 26)))
	}
	fmt.Fprintln(&b, styleMuted.Render("↑/↓ history  tab complete"))
	fmt.Fprintln(&b, styleMuted.Render("drag  select text"))
	fmt.Fprintln(&b, styleMuted.Render("^y copy selection/reply"))
	fmt.Fprintln(&b, styleMuted.Render("^t copy chat"))
	fmt.Fprintln(&b, styleMuted.Render("^o/click expand tool"))
	fmt.Fprintln(&b, styleMuted.Render("alt+↑/↓ select tool"))
	if m.terminal != nil {
		fmt.Fprintln(&b, styleMuted.Render("^] sandbox shell"))
	}
	fmt.Fprintln(&b, styleMuted.Render("pgup/pgdn scroll"))
	fmt.Fprintln(&b, styleMuted.Render("^home/^end top/btm"))
	fmt.Fprintln(&b, styleMuted.Render("wheel  scroll"))
	fmt.Fprintln(&b, styleMuted.Render("/exit  quit (or /help)"))
	fmt.Fprintln(&b, styleMuted.Render("^c quit"))
	return b.String()
}

// runningJobs counts the sandbox's shell jobs still running: ones the model
// left in the background keep holding the sandbox's processes after the turn.
func runningJobs(runner executor.Executor) int {
	running := 0
	for _, job := range runner.Jobs() {
		if job.Running {
			running++
		}
	}
	return running
}

// renderApprovalStanding is the sidebar's account of what runs without asking.
// Anything the operator has waved through is highlighted: it stays on for the
// rest of the session, long after the keypress that turned it on has scrolled
// out of mind.
func renderApprovalStanding(standing agent.ApprovalStanding) string {
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render("Approval"))
	switch {
	case !standing.Required:
		fmt.Fprintln(&b, styleMuted.Render("not required"))
	case standing.All:
		fmt.Fprintln(&b, styleAccent("auto: all tools"))
	case len(standing.Tools) > 0:
		fmt.Fprintln(&b, styleAccent("auto:"))
		for _, toolName := range standing.Tools {
			fmt.Fprintln(&b, styleAccent(truncate("  "+toolName, 26)))
		}
	default:
		fmt.Fprintln(&b, "asks every call")
	}
	return b.String()
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

// spendWarnFraction is how close to a spending limit a session gets before the
// operator is told, early enough to decide whether the task is worth the rest.
const spendWarnFraction = 0.8

// renderSessionSpend is the sidebar's session total and cost, each shown
// against its limit when one is set, and highlighted once the session is close
// to whichever limit it will reach first.
func renderSessionSpend(stats governor.Stats) string {
	tokens := fmt.Sprintf("tot: %d", stats.TotalTokens)
	if stats.Limits.MaxTokens > 0 {
		tokens += " / " + humanTokens(stats.Limits.MaxTokens)
	}
	cost := "$ N/A"
	if stats.CostAvailable {
		cost = fmt.Sprintf("$%.4f", stats.EstimatedCostUSD)
	}
	if stats.Limits.MaxCostUSD > 0 {
		cost += fmt.Sprintf(" / $%.2f", stats.Limits.MaxCostUSD)
	}

	highlight := func(line string) string { return line }
	if stats.LimitUsed() >= spendWarnFraction {
		highlight = styleAccent
	}
	costLine := highlight(cost)
	if !stats.CostAvailable {
		costLine = styleMuted.Render(cost)
	}
	return highlight(tokens) + "\n" + costLine + "\n"
}

// noteSpend warns in the conversation when the session nears a spending limit,
// once per approach: the sidebar figure is easy to miss on a long autonomous
// run, and the limit otherwise arrives as a turn that stops without warning. It
// also says, once, when a cost limit is set that the provider gives it nothing
// to hold against.
func (m *Model) noteSpend(stats governor.Stats) {
	used := stats.LimitUsed()
	switch {
	case used < spendWarnFraction:
		m.spendWarned = false
	case !m.spendWarned:
		m.spendWarned = true
		m.appendLine(styleWarn.Render(fmt.Sprintf(
			"⚠ this session has used %d%% of its spending limit — at 100%% the agent stops sending requests; /new starts a fresh session",
			int(used*100))))
	}

	if stats.CostLimitUnenforced() && !m.costLimitWarned {
		m.costLimitWarned = true
		m.appendLine(styleWarn.Render(fmt.Sprintf(
			"⚠ provider.max_session_cost_usd is $%.2f, but this provider reports no cost, so it cannot be enforced — set provider.max_session_tokens to cap the session",
			stats.Limits.MaxCostUSD)))
	}
}

// showSpawnedSession opens the chat of a spawned session the way it began:
// where it came from, then the briefing it was handed, if there was one.
func (m *Model) showSpawnedSession(parent, child session.ID, briefing string) {
	m.appendLine(styleMuted.Render(renderSessionSpawned(parent, child)))
	if briefing != "" {
		m.appendLine(styleMuted.Render("Briefing:"))
		m.appendReply(briefing)
	}
}

func renderSessionSpawned(parent, child session.ID) string {
	line := "✦ spawned session"
	if child != "" {
		line += " " + string(child)
	}
	if parent != "" {
		line += " from " + string(parent)
	}
	return line
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

// truncate fits s on one line of n cells, then the ellipsis.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "") + "…"
}
