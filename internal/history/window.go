// Package history bounds a conversation to a token budget before it is sent to
// the model.
//
// The whole conversation is re-sent on every request, so its size is the one
// quantity in the harness that grows without an upper bound: left alone it ends
// a session permanently, because once the history alone exceeds the model's
// context window every further request fails, including a trivial one.
//
// Reduction happens in two stages with deliberately different guarantees.
// Windowing drops the oldest whole exchanges, which is lossy but keeps the
// remaining conversation exactly as it was. Shrinking rewrites tool results
// inside the part that must be kept, which preserves the shape of the exchange
// at the cost of its detail, and only runs when a single turn is too large to
// fit on its own.
//
// Everything here is a pure function of its arguments: callers own the budget,
// the statistics, and the decision of what to tell the operator.
package history

import (
	"fmt"

	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/toolout"
)

// Budget is the token ceiling for one request. Windowing starts when a prompt
// exceeds High and then removes enough to bring it under Low.
//
// The gap between the two is what keeps the retained prefix stable: a window
// that trimmed back to exactly High would advance again on the very next
// request, and a prompt prefix that changes every request is one no provider
// can serve from its cache.
type Budget struct {
	High int
	Low  int
}

// lowWaterPercent is how far below the ceiling a window advance reaches. Low
// enough that an advance buys several requests of headroom, high enough that it
// does not throw away most of the conversation to buy them.
const lowWaterPercent = 70

// BudgetFor derives a Budget from the configured prompt ceiling. A ceiling of
// zero or less disables windowing.
func BudgetFor(maxPromptTokens int) Budget {
	if maxPromptTokens <= 0 {
		return Budget{}
	}
	return Budget{High: maxPromptTokens, Low: maxPromptTokens * lowWaterPercent / 100}
}

// Enabled reports whether this budget windows anything.
func (b Budget) Enabled() bool { return b.High > 0 }

// Stats describes one call to Window.
type Stats struct {
	// BeforeTokens is the whole conversation plus the tool definitions;
	// AfterTokens is what Window actually returned.
	BeforeTokens int
	AfterTokens  int

	// DroppedMessages counts every message the window has left behind so far,
	// not just the ones this call dropped. ShrunkResults counts the tool
	// results this call rewrote.
	DroppedMessages int
	ShrunkResults   int

	// DropThrough is the index the window has advanced to. Feed it back into
	// the next call: it is what holds the retained prefix still between
	// advances.
	DropThrough int

	// FixedTokens is what the request costs before any of the conversation:
	// the system prompt, a summary pinned behind it, and the tool definitions.
	// A budget below this can never be met, which is the one thing the operator
	// needs told when it cannot be.
	FixedTokens int

	// OverBudget reports that the request still exceeds the ceiling with
	// nothing left to remove.
	OverBudget bool
}

// Advanced reports whether this call moved the window, as opposed to reusing
// the position it was given. A conversation that has never been windowed
// reports a DropThrough of its pinned length rather than zero, so the position
// alone does not distinguish the two.
func (s Stats) Advanced(from int) bool { return s.DroppedMessages > 0 && s.DropThrough > from }

// Window reduces msgs to fit budget and returns the result along with what it
// had to do to get there. dropThrough is the position returned by the previous
// call for the same conversation, or zero for a conversation that has never
// been windowed.
//
// Three things are never removed: the system prompt, any summary pinned behind
// it, and the turn currently in flight — everything from the last user message
// onward. Whatever is left is dropped one exchange at a time, an assistant
// message together with the tool results answering it, so a tool_call is never
// separated from its reply.
//
// msgs is never modified: a reduced result is assembled from copies.
func Window(msgs []llm.Message, tools []llm.ToolDefinition, budget Budget, dropThrough int) ([]llm.Message, Stats) {
	if !budget.Enabled() {
		total := Estimate(msgs, tools)
		return msgs, Stats{BeforeTokens: total, AfterTokens: total}
	}
	toolTokens := estimateTools(tools)

	suffix := make([]int, len(msgs)+1)
	for i := len(msgs) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + EstimateMessage(msgs[i])
	}

	pinned := leadingSystemRun(msgs)
	tailStart := protectedTailStart(msgs, pinned)
	dropThrough = clamp(dropThrough, pinned, tailStart)

	pinnedTokens := suffix[0] - suffix[pinned] + toolTokens
	retained := func(through int) int {
		total := pinnedTokens + suffix[through]
		if through > pinned {
			total += EstimateMessage(dropMarker(through - pinned))
		}
		return total
	}

	stats := Stats{BeforeTokens: suffix[0] + toolTokens, FixedTokens: pinnedTokens}

	if retained(dropThrough) > budget.High {
		for dropThrough < tailStart && retained(dropThrough) > budget.Low {
			dropThrough = exchangeEnd(msgs, dropThrough, tailStart)
		}
	}

	out := assemble(msgs, pinned, dropThrough)
	stats.DropThrough = dropThrough
	stats.DroppedMessages = dropThrough - pinned
	stats.AfterTokens = retained(dropThrough)

	if stats.AfterTokens > budget.High {
		stats.AfterTokens, stats.ShrunkResults = shrinkOldestResults(out, stats.AfterTokens, budget.Low)
		stats.OverBudget = stats.AfterTokens > budget.High
	}
	return out, stats
}

// assemble builds the message list actually sent: the pinned head, a marker
// standing in for whatever the window has dropped, then the rest.
func assemble(msgs []llm.Message, pinned, dropThrough int) []llm.Message {
	out := make([]llm.Message, 0, len(msgs)-(dropThrough-pinned)+1)
	out = append(out, msgs[:pinned]...)
	if dropThrough > pinned {
		out = append(out, dropMarker(dropThrough-pinned))
	}
	return append(out, msgs[dropThrough:]...)
}

// dropMarker is what the model is told in place of the messages it can no
// longer see. Without it the model answers from a conversation it believes it
// still remembers.
func dropMarker(dropped int) llm.Message {
	return llm.Message{
		Role: llm.RoleSystem,
		Content: fmt.Sprintf("[%d earlier messages were removed from this conversation to stay within the "+
			"context budget. You can no longer see them: ask the user rather than answering from what you "+
			"assume was said.]", dropped),
	}
}

// CompactableSpan returns the half-open range of msgs a summary may stand in
// for: everything after the system prompt, up to a boundary that leaves the
// conversation valid and its most recent part readable.
//
// The preferred boundary is the last user message, which keeps the whole final
// turn verbatim. One long turn can fill a context on its own, though — an
// agentic turn is a single user message followed by twenty tool calls — and
// there the last user message is the second message in the session, leaving
// nothing to summarise at exactly the moment summarising is the only way out.
// So when that boundary yields nothing, the span extends to the last exchange
// instead, keeping the final assistant message and its results.
//
// A summary left by an earlier compaction falls inside the range on purpose.
// Windowing pins it so it is never dropped, but compacting again folds it into
// the new summary, so repeated compaction leaves one summary rather than a
// growing stack of them.
func CompactableSpan(msgs []llm.Message) (from, through int) {
	if len(msgs) > 0 && msgs[0].Role == llm.RoleSystem {
		from = 1
	}
	through = protectedTailStart(msgs, from)
	if through-from < MinCompactableMessages {
		through = lastExchangeStart(msgs, from)
	}
	return from, through
}

// MinCompactableMessages is the shortest span worth summarising: below it a
// compaction would cost a model call to save nothing.
const MinCompactableMessages = 2

// lastExchangeStart returns the index of the last message that is not a tool
// result, so the exchange it belongs to stays whole. Retaining from anywhere
// later would open the conversation with a reply to a call the model can no
// longer see.
func lastExchangeStart(msgs []llm.Message, floor int) int {
	for i := len(msgs) - 1; i >= floor; i-- {
		if msgs[i].Role != llm.RoleTool {
			return i
		}
	}
	return floor
}

// leadingSystemRun returns the length of the run of system messages the
// conversation opens with: the system prompt, plus any summary a compaction has
// pinned behind it.
func leadingSystemRun(msgs []llm.Message) int {
	run := 0
	for run < len(msgs) && msgs[run].Role == llm.RoleSystem {
		run++
	}
	return run
}

// protectedTailStart returns the index of the turn in flight — the last user
// message and everything after it. Dropping into that span would take away the
// instruction the model is working on or the tool results it just received, so
// the window stops here even when that leaves the request over budget.
func protectedTailStart(msgs []llm.Message, pinned int) int {
	for i := len(msgs) - 1; i >= pinned; i-- {
		if msgs[i].Role == llm.RoleUser {
			return i
		}
	}
	return pinned
}

// exchangeEnd returns the index just past the exchange beginning at start: one
// message plus every tool result answering it. Advancing the window a whole
// exchange at a time is what keeps a tool_call from being separated from its
// reply, which providers reject.
func exchangeEnd(msgs []llm.Message, start, limit int) int {
	if start >= limit {
		return start
	}
	end := start + 1
	for end < limit && msgs[end].Role == llm.RoleTool {
		end++
	}
	return end
}

// shrinkOldestResults is the last resort, reached only when the turn in flight
// is too large to fit on its own: twenty-five iterations of tool output can do
// it. Tool results are rewritten in place, oldest first — the message, its
// tool_call_id, and the call that produced it all survive, so the exchange
// stays valid and the model can still see that it ran the tool. Only the output
// is lost.
//
// It runs in passes, each keeping less than the last, and stops the moment the
// request fits. Giving up the detail of an old result beats abandoning a turn
// that is otherwise going fine.
//
// msgs holds copies made by assemble, so this never reaches the caller's
// conversation.
func shrinkOldestResults(msgs []llm.Message, current, target int) (int, int) {
	rewritten := make([]bool, len(msgs))
	shrunk := 0
	for _, keep := range shrinkPasses {
		for i := range msgs {
			if current <= target {
				return current, shrunk
			}
			if msgs[i].Role != llm.RoleTool {
				continue
			}
			reduced := shrinkResult(msgs[i].Content, keep)
			if len(reduced) >= len(msgs[i].Content) {
				continue
			}
			before := EstimateMessage(msgs[i])
			msgs[i].Content = reduced
			current += EstimateMessage(msgs[i]) - before
			if !rewritten[i] {
				rewritten[i] = true
				shrunk++
			}
		}
	}
	return current, shrunk
}

// shrinkPasses is how much of a result each pass keeps: first enough to say
// what the call produced, then nothing but the fact that it ran.
var shrinkPasses = []int{512, 0}

func shrinkResult(content string, keep int) string {
	if keep <= 0 {
		return droppedResultNote
	}
	reduced, _ := toolout.Truncate(content, keep, shrunkResultNote)
	return reduced
}

const shrunkResultNote = "dropped to stay within the context budget; re-run the tool if you need it again"

const droppedResultNote = "[this tool ran, but its output was dropped entirely to stay within the context budget; re-run it if you need what it said]"

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
