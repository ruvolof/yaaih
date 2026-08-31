// Compaction: trading the earlier part of a conversation for a summary of it.
//
// The automatic window keeps a session alive by dropping what no longer fits,
// which costs the model everything it dropped. Compaction is the operator's
// answer to that: one model call that reads the span about to be lost and
// writes down what mattered, so the agent carries the gist forward instead of
// nothing.
package agent

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync/atomic"

	"github.com/ruvolof/aibitat-harness-go/internal/history"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
	"github.com/ruvolof/aibitat-harness-go/internal/toolout"
)

// compactPrompt instructs the summarising call. It asks for notes the agent
// will read as its own memory, not for a report to the user: the summary
// arrives in the conversation as something the agent already knows.
const compactPrompt = `You are compressing the earlier part of a conversation between a user and an AI agent, so the agent can keep working after that part is discarded.

Preserve:
- what the user asked for, in their own words where the wording matters
- decisions taken, and why
- facts established: paths, names, versions, identifiers, error messages, results worth keeping
- what has already been done, and what is still outstanding

Leave out pleasantries, attempts that led nowhere, and output that has since been superseded. Write it as notes for your own use rather than as a report. There is no length limit, but every line has to earn its place.`

// compactSummaryPrefix marks the summary in the conversation, so the model
// reads it as a record of what happened rather than as an instruction.
const compactSummaryPrefix = "[Summary of the earlier part of this conversation, which is no longer available in full]"

// Compact replaces the earlier part of the conversation with a summary of it,
// leaving the system prompt and the most recent turn untouched. instructions,
// when non-empty, is the operator asking for a particular emphasis.
//
// Serialised against turns: it rewrites the conversation a turn would be
// reading. Nothing is replaced unless the summary arrives intact, so a failed
// or stopped compaction leaves the session exactly as it was.
func (a *Agent) Compact(ctx context.Context, instructions string) {
	if !a.turnMu.TryLock() {
		log.Warn("Compact called while a turn is in progress")
		a.publish(ErrorMsg{Err: "agent is still working on the previous turn — please wait for it to finish"})
		return
	}
	defer a.turnMu.Unlock()

	// Compaction occupies the UI exactly as a turn does, so it ends like one:
	// a single AssistantDoneMsg on every path out, including the ones that
	// change nothing.
	turnEnd := AssistantDoneMsg{}
	defer func() { a.publish(turnEnd) }()

	defer func() {
		if r := recover(); r != nil {
			log.Error("Compact panic", "value", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			a.publish(ErrorMsg{Err: fmt.Sprintf("panic while compacting: %v (see logs/)", r)})
		}
	}()

	messages := a.snapshotMessages()
	from, through := history.CompactableSpan(messages)
	if through-from < history.MinCompactableMessages {
		a.publish(ErrorMsg{Err: "there is nothing to compact yet — this session is barely past its system prompt"})
		return
	}

	log.Info("compacting", "messages", through-from, "history_msgs", len(messages))
	summary, usage, err := a.summarise(ctx, messages[from:through], instructions)
	if err != nil {
		if ctx.Err() != nil {
			log.Info("compaction cancelled by operator")
			turnEnd.Cancelled = true
			return
		}
		log.Error("compaction failed", "err", err)
		a.publish(ErrorMsg{Err: fmt.Sprintf("compaction failed, conversation unchanged: %v", err)})
		return
	}
	if strings.TrimSpace(summary) == "" {
		a.publish(ErrorMsg{Err: "the model returned an empty summary — conversation unchanged"})
		return
	}

	a.gov.Record(usage)
	a.globGov.Record(usage)
	if usage.CostAvailable {
		a.gov.AddCost(usage.Cost)
		a.globGov.AddCost(usage.Cost)
	}

	before := history.Estimate(messages, nil)
	after := a.replaceWithSummary(from, through, compactSummaryPrefix+"\n\n"+summary)

	a.mu.Lock()
	calibration := a.calibration
	a.mu.Unlock()

	// The compacted conversation is what the next request will carry, so the
	// gauge moves to it now rather than staying blank until the operator sends
	// something.
	a.publish(a.statsUpdate(calibration.Apply(after)))
	a.publish(HistoryTrimmedMsg{
		DroppedMessages: through - from,
		BeforeTokens:    calibration.Apply(before),
		AfterTokens:     calibration.Apply(after),
		Summarised:      true,
	})
}

// summarise runs the one-off model call that reads the span and writes the
// summary. The deltas go to the event stream like any other reply, so the
// operator watches it being written instead of waiting on a silent turn.
func (a *Agent) summarise(ctx context.Context, span []llm.Message, instructions string) (string, llm.TokenUsage, error) {
	request := strings.Builder{}
	request.WriteString("Conversation to summarise:\n\n")
	request.WriteString(a.renderSpan(span))
	if instructions != "" {
		request.WriteString("\n\nThe user asks you to pay particular attention to: " + instructions)
	}

	var streamed strings.Builder
	resp, err := a.prov.StreamChat(ctx, &llm.ChatRequest{
		Model: a.cfg.Model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: compactPrompt},
			{Role: llm.RoleUser, Content: request.String()},
		},
	}, func(ev llm.StreamEvent) {
		if ev.DeltaText != "" {
			streamed.WriteString(ev.DeltaText)
			a.publish(AssistantDeltaMsg{Text: ev.DeltaText})
		}
	})
	if err != nil {
		return "", llm.TokenUsage{}, err
	}
	if resp.Content != "" {
		return resp.Content, resp.Usage, nil
	}
	return streamed.String(), resp.Usage, nil
}

// renderSpan writes the conversation out for the summarising model to read. It
// is bounded, because the span being summarised is by definition the part that
// grew too large to send.
func (a *Agent) renderSpan(span []llm.Message) string {
	var b strings.Builder
	for _, msg := range span {
		switch msg.Role {
		case llm.RoleSystem:
			fmt.Fprintf(&b, "EARLIER SUMMARY: %s\n\n", msg.Content)
		case llm.RoleUser:
			fmt.Fprintf(&b, "USER: %s\n\n", msg.Content)
		case llm.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				fmt.Fprintf(&b, "ASSISTANT: %s\n\n", msg.Content)
			}
			for _, call := range msg.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT CALLS %s(%s)\n\n", call.Name, call.Arguments)
			}
		case llm.RoleTool:
			fmt.Fprintf(&b, "RESULT OF %s: %s\n\n", msg.Name, msg.Content)
		}
	}
	rendered, _ := toolout.Truncate(b.String(), a.summaryInputBytes(), "the middle of the conversation was too large to summarise in full")
	return rendered
}

// summaryInputBytes keeps the summarising call itself inside the budget, at a
// byte-per-token ratio pessimistic enough to cover a transcript full of JSON
// and with room left for the summary that comes back.
func (a *Agent) summaryInputBytes() int {
	return a.cfg.Options.MaxContextTokens * 2
}

// replaceWithSummary swaps the span for the summary in the live conversation
// and records the swap in the session file, returning the estimated size of
// what is left.
//
// The window's position is dropped along with the messages it indexed: the
// conversation it referred to no longer exists.
func (a *Agent) replaceWithSummary(from, through int, summary string) int {
	message := llm.Message{Role: llm.RoleSystem, Content: summary}

	a.mu.Lock()
	compacted := make([]llm.Message, 0, len(a.messages)-(through-from)+1)
	compacted = append(compacted, a.messages[:from]...)
	compacted = append(compacted, message)
	compacted = append(compacted, a.messages[through:]...)
	a.messages = compacted
	a.dropThrough = 0
	a.shrunkResults = 0
	writer := a.writer
	a.mu.Unlock()

	if writer != nil {
		seq := int(atomic.AddInt64(&a.seq, 1))
		err := writer.AppendCompaction(session.Compaction{
			Seq:     seq,
			From:    from,
			Through: through,
			Role:    string(message.Role),
			Content: message.Content,
		})
		if err != nil {
			log.Warn("session compaction append failed", "err", err, "seq", seq)
		}
	}

	remaining := history.Estimate(compacted, nil)
	log.Info("compacted", "replaced", through-from, "history_msgs", len(compacted), "tokens", remaining)
	return remaining
}

func (a *Agent) currentCWD() string {
	if a.runner == nil {
		return ""
	}
	return a.runner.CWD()
}
