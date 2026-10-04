// TUI-bound message types emitted by the agent loop. These are pure data —
// the TUI knows how to render each one. Keeping them here (rather than in the
// tui package) lets the agent be unit-tested without dragging BubbleTea in.
package agent

import (
	"github.com/ruvolof/yaaih/internal/governor"
)

// AssistantDeltaMsg is one streamed content chunk from the model.
type AssistantDeltaMsg struct {
	Text string
}

// AssistantMessageMsg fires when the model has finished one assistant message.
// A turn is a sequence of these: every message but the last carries tool calls
// and is followed by the loop running them. It marks the point where the text
// streamed so far belongs in the transcript — before the tool calls that text
// introduces, rather than after everything the turn goes on to do.
type AssistantMessageMsg struct{}

// AssistantDoneMsg fires when the model has finished a turn (any tool calls
// will have already been executed and folded back in). Cancelled distinguishes
// a turn the operator stopped from one the model ended on its own.
type AssistantDoneMsg struct {
	Cancelled bool
}

// ToolCallStartMsg announces an imminent tool invocation. ID pairs it with the
// ToolCallResultMsg that answers it.
type ToolCallStartMsg struct {
	ID   string
	Name string
	Args string
}

// ToolCallResultMsg carries the reply to a tool call. Result is the reply
// exactly as the model receives it — normalised, bounded, and with the
// "error:" prefix when the call failed — not the tool's raw output. Err is a
// short reason for the failure, empty on success.
type ToolCallResultMsg struct {
	ID     string
	Name   string
	Result string
	Err    string
}

// TimeoutMsg is rendered as a prominent warning in the TUI.
type TimeoutMsg struct {
	Command string
	Message string
}

// StatsUpdateMsg refreshes the sidebar's token/cost totals: Session is the
// current session's usage, Global is the whole run's usage. ContextTokens is
// what the last request cost against ContextBudget, so the operator can see the
// window filling before it starts dropping.
type StatsUpdateMsg struct {
	Session       governor.Stats
	Global        governor.Stats
	CWD           string
	ContextTokens int
	ContextBudget int
}

// HistoryTrimmedMsg reports that part of the conversation stopped being sent to
// the model. Summarised distinguishes an operator-invoked /compact, whose gist
// survives, from the automatic window, whose contents are simply gone.
type HistoryTrimmedMsg struct {
	DroppedMessages int
	ShrunkResults   int
	BeforeTokens    int
	AfterTokens     int
	Summarised      bool
}

// ApprovalRequestMsg asks the operator to permit a restricted tool call.
type ApprovalRequestMsg struct {
	Request *ApprovalRequest
}

// ErrorMsg is a non-fatal error to surface in the chat view.
type ErrorMsg struct {
	Err string
}
