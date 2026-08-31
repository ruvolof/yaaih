// Package agent ties the LLM, tool registry, gatekeeper, governor, and
// executor together into the core "input → model → tools → model → ..." loop.
//
// The agent is decoupled from the TUI: it emits typed events on an Events
// channel, and the TUI translates them into BubbleTea messages.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruvolof/aibitat-harness-go/internal/browser"
	"github.com/ruvolof/aibitat-harness-go/internal/config"
	"github.com/ruvolof/aibitat-harness-go/internal/executor"
	"github.com/ruvolof/aibitat-harness-go/internal/governor"
	"github.com/ruvolof/aibitat-harness-go/internal/history"
	"github.com/ruvolof/aibitat-harness-go/internal/llm"
	"github.com/ruvolof/aibitat-harness-go/internal/logging"
	"github.com/ruvolof/aibitat-harness-go/internal/session"
	"github.com/ruvolof/aibitat-harness-go/internal/toolout"
)

var log = logging.For("agent")

// Event is the closed set of message types the agent emits. The TUI handles
// these by type-switching.
type Event interface{ isAgentEvent() }

func (AssistantDeltaMsg) isAgentEvent()   {}
func (AssistantMessageMsg) isAgentEvent() {}
func (AssistantDoneMsg) isAgentEvent()    {}
func (ToolCallStartMsg) isAgentEvent()    {}
func (ToolCallResultMsg) isAgentEvent()   {}
func (TimeoutMsg) isAgentEvent()          {}
func (StatsUpdateMsg) isAgentEvent()      {}
func (ApprovalRequestMsg) isAgentEvent()  {}
func (HistoryTrimmedMsg) isAgentEvent()   {}
func (ErrorMsg) isAgentEvent()            {}

// Agent owns the conversation state.
type Agent struct {
	cfg      *config.Config
	prov     llm.Provider
	gov      *governor.Governor // session-scoped usage (resets on /new, restored on resume)
	globGov  *governor.Governor // run-scoped usage, shared across agent switches; never reset
	runner   executor.Executor
	br       *browser.Browser
	gate     *Gatekeeper
	registry *Registry

	// turnMu serialises whole conversation turns. Held for the entire body
	// of Send so a second user message can't start a turn while the first
	// is still in flight (which would race on messages history, run two LLM
	// calls in parallel with an unanswered tool_call in the prompt, and
	// queue up on the executor's command mutex).
	turnMu sync.Mutex

	mu       sync.Mutex
	messages []llm.Message

	// dropThrough is how far the context window has advanced into messages,
	// shrunkResults how many tool results it has had to rewrite to fit, and
	// calibration the estimator's correction against the token counts the
	// provider reports. All three are read and written on the turn loop's path
	// to building a request.
	dropThrough   int
	shrunkResults int
	calibration   history.Calibration

	// writer persists the transcript to a JSONL session file. May be nil when
	// persistence is disabled. seq is the monotonic message sequence number;
	// it continues from the seed length on a resumed session. sessionsDir is
	// where this agent's transcripts live, so the agent can create and load its
	// own sessions for the /new, /resume, /save commands.
	writer      *session.Writer
	seq         int64
	sessionsDir string

	// events carries the agent's output to the TUI. Everything that writes to
	// it goes through publish, which holds eventsMu in read mode; Close takes
	// it in write mode, so the channel is never closed underneath a publisher.
	// closing is shut first, releasing publishers that are blocked on a channel
	// nobody reads any more — a turn outlives the UI that was watching it.
	events       chan Event
	eventsMu     sync.RWMutex
	closing      chan struct{}
	eventsClosed bool
}

// New builds an Agent. When seed is non-empty it is used as the starting
// conversation (the resume path; seed already includes the system message and
// is assumed to already be on disk); otherwise the agent is seeded with the
// system prompt and that message is persisted as the first entry. writer may
// be nil to disable persistence.
func New(
	cfg *config.Config,
	prov llm.Provider,
	gov *governor.Governor,
	globGov *governor.Governor,
	runner executor.Executor,
	br *browser.Browser,
	gate *Gatekeeper,
	writer *session.Writer,
	seed []llm.Message,
	sessionsDir string,
) *Agent {
	var msgs []llm.Message
	if len(seed) > 0 {
		msgs = append(msgs, seed...)
	} else {
		msgs = []llm.Message{{Role: llm.RoleSystem, Content: cfg.SystemPrompt}}
	}
	a := &Agent{
		cfg:         cfg,
		prov:        prov,
		gov:         gov,
		globGov:     globGov,
		runner:      runner,
		br:          br,
		gate:        gate,
		registry:    NewRegistry(cfg, runner, br),
		writer:      writer,
		sessionsDir: sessionsDir,
		events:      make(chan Event, 32),
		closing:     make(chan struct{}),
		messages:    msgs,
	}
	if len(seed) > 0 {
		// Resumed: the seed is already on disk; continue numbering after it.
		a.seq = int64(len(seed))
	} else {
		// Fresh: persist the system message as seq 1.
		a.persist(msgs[0], nil)
	}
	// Forward gatekeeper requests onto the event stream so the TUI sees them.
	go func() {
		defer logging.Recover("agent.gatekeeper-forwarder")
		for req := range gate.Requests() {
			a.publish(ApprovalRequestMsg{Request: req})
		}
	}()
	return a
}

// Events is the read end of the event channel. Closed when the agent stops.
func (a *Agent) Events() <-chan Event { return a.events }

// publish hands one event to whoever is reading the event stream. It is the
// only writer of a.events, and it is a no-op once the agent has been closed:
// a turn can outlive the UI that started it, and it must be able to unwind
// without a reader and without a channel to publish to.
func (a *Agent) publish(ev Event) {
	a.eventsMu.RLock()
	defer a.eventsMu.RUnlock()
	if a.eventsClosed {
		return
	}
	select {
	case a.events <- ev:
	case <-a.closing:
	}
}

// Registry exposes the tool registry so callers can inject additional tools
// (e.g. script tools) before the turn loop starts.
func (a *Agent) Registry() *Registry { return a.registry }

// History returns a snapshot of the conversation so far, including the system
// message. The TUI uses this to repopulate the chat view when resuming a
// session; on a fresh start it holds only the system prompt.
func (a *Agent) History() []llm.Message { return a.snapshotMessages() }

// CurrentSession returns the active session id, or "" when persistence is off.
func (a *Agent) CurrentSession() session.ID {
	writer := a.currentWriter()
	if writer == nil {
		return ""
	}
	return writer.ID()
}

// currentWriter reads the session writer under the lock that /new and /resume
// swap it behind, so a caller never works with a writer that has been retired
// half way through what it was doing.
func (a *Agent) currentWriter() *session.Writer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writer
}

// SessionsDir is the directory where this agent's session files are stored.
func (a *Agent) SessionsDir() string { return a.sessionsDir }

// SessionStats is the usage accumulated by the current session (resets on /new,
// restored on resume). GlobalStats is the usage accumulated across the whole
// run, including prior agents in this process.
func (a *Agent) SessionStats() governor.Stats { return a.gov.Snapshot() }
func (a *Agent) GlobalStats() governor.Stats  { return a.globGov.Snapshot() }

// errTurnInFlight is what the session-switching operations report rather than
// waiting for the turn: they are called from the UI thread, and that thread is
// also the one draining the events the turn is publishing.
var errTurnInFlight = errors.New("a turn is still running — stop it or wait for it to finish")

// NewSession finalises the current session and starts a fresh, empty one
// seeded only with the system prompt. Returns the new session id. Refused
// while a turn is in flight: it would replace the conversation the turn is
// writing into.
func (a *Agent) NewSession() (session.ID, error) {
	if !a.turnMu.TryLock() {
		return "", errTurnInFlight
	}
	defer a.turnMu.Unlock()

	id := session.NewID()
	w, err := session.NewWriter(a.sessionsDir, a.headerFor(id))
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	a.closeWriter("switched")
	a.resetConversation(w, nil)
	log.Info("started new session", "id", id)
	return id, nil
}

// ResumeSession finalises the current session and loads the one named by id,
// replacing the conversation. Returns the loaded history for the UI to render.
// Refused while a turn is in flight, for the same reason as NewSession.
func (a *Agent) ResumeSession(id session.ID) ([]llm.Message, error) {
	if err := session.ValidateID(id); err != nil {
		return nil, err
	}
	if !a.turnMu.TryLock() {
		return nil, errTurnInFlight
	}
	defer a.turnMu.Unlock()

	_, msgs, usage, err := session.Load(a.sessionsDir, id)
	if err != nil {
		return nil, err
	}
	w, err := session.OpenForAppend(a.sessionsDir, id)
	if err != nil {
		return nil, fmt.Errorf("reopen %s: %w", id, err)
	}
	a.closeWriter("switched")
	a.resetConversation(w, msgs)
	// Restore the resumed session's accumulated usage (resetConversation zeroed
	// it). Only the session governor — the run total is not rewound.
	a.gov.Seed(usage.InputTokens, usage.OutputTokens, usage.Cost, usage.CostAvailable)
	log.Info("resumed session", "id", id, "messages", len(msgs))
	return msgs, nil
}

// RenameSession renames the active session file, keeping it active. Returns an
// error if there is no active session.
func (a *Agent) RenameSession(newID session.ID) error {
	writer := a.currentWriter()
	if writer == nil {
		return errors.New("no active session to rename")
	}
	if err := writer.Rename(a.sessionsDir, newID); err != nil {
		return err
	}
	log.Info("renamed session", "id", newID)
	return nil
}

// headerFor builds a session header from the current config.
func (a *Agent) headerFor(id session.ID) session.Header {
	return session.Header{
		ID:        id,
		Started:   time.Now().UTC().Format(time.RFC3339),
		AgentPath: a.cfg.SourcePath,
		Model:     a.cfg.Model,
		Provider:  a.cfg.Provider.BaseURL,
	}
}

// closeWriter finalises the current session writer (if any) with the given
// reason and detaches it.
func (a *Agent) closeWriter(reason string) {
	a.mu.Lock()
	writer := a.writer
	a.writer = nil
	a.mu.Unlock()

	if writer == nil {
		return
	}
	if err := writer.Close(reason); err != nil {
		log.Warn("session close failed", "err", err)
	}
}

// resetConversation replaces the conversation history, the session writer, and
// the sequence counter. With an empty seed it starts fresh from the system
// prompt and persists it as the first entry; otherwise it adopts seed as-is
// (already on disk) and continues numbering after it.
func (a *Agent) resetConversation(w *session.Writer, seed []llm.Message) {
	a.mu.Lock()
	if len(seed) > 0 {
		a.messages = append([]llm.Message(nil), seed...)
		atomic.StoreInt64(&a.seq, int64(len(seed)))
	} else {
		a.messages = []llm.Message{{Role: llm.RoleSystem, Content: a.cfg.SystemPrompt}}
		atomic.StoreInt64(&a.seq, 0)
	}
	a.writer = w
	// The window's position and what it had to shorten index the conversation
	// that just went away. The estimator's correction is not reset with them:
	// that is a property of the model, which has not changed.
	a.dropThrough = 0
	a.shrunkResults = 0
	a.mu.Unlock()
	// The session-scoped usage restarts from zero; ResumeSession reseeds it from
	// the loaded transcript afterwards. The global (run) governor is untouched.
	a.gov.Reset()
	if len(seed) == 0 {
		a.persist(a.messages[0], nil) // seq -> 1
	}
}

// Close releases the event channel and finalises the session file. Tool
// resources are owned by main(). The gatekeeper is closed too, which stops the
// forwarder goroutine so it does not leak across agent switches.
//
// A turn in flight is waited out rather than torn through: it still owns the
// tail of the transcript, and closing the event channel underneath it would
// leave it publishing into a channel that no longer exists. Shutting closing
// first is what lets it get that far — by now nothing is reading the events it
// publishes on the way out, so without that it would block on a full channel
// and never release the turn.
func (a *Agent) Close() {
	close(a.closing)
	a.turnMu.Lock()
	defer a.turnMu.Unlock()

	a.gate.Close()

	a.eventsMu.Lock()
	a.eventsClosed = true
	close(a.events)
	a.eventsMu.Unlock()

	// The writer stays attached: the session it wrote is still the one the
	// agent belongs to, and callers ask for its id after teardown.
	if writer := a.currentWriter(); writer != nil {
		if err := writer.Close("clean"); err != nil {
			log.Warn("session close failed", "err", err)
		}
	}
}

// record appends m to the conversation history and persists it to the session
// file. Thread-safe.
func (a *Agent) record(m llm.Message) { a.recordUsage(m, nil) }

// recordUsage is record with the LLM usage that produced m (assistant turns),
// so the session file can restore per-session cost on resume.
func (a *Agent) recordUsage(m llm.Message, usage *session.Usage) {
	a.mu.Lock()
	a.messages = append(a.messages, m)
	a.mu.Unlock()
	a.persist(m, usage)
}

// persist writes one message to the session file, assigning the next sequence
// number. No-op when persistence is disabled.
func (a *Agent) persist(m llm.Message, usage *session.Usage) {
	writer := a.currentWriter()
	if writer == nil {
		return
	}
	seq := int(atomic.AddInt64(&a.seq, 1))
	err := writer.Append(session.Entry{
		Seq:        seq,
		Role:       string(m.Role),
		Content:    m.Content,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
		Usage:      usage,
	})
	if err != nil {
		log.Warn("session append failed", "err", err, "seq", seq)
	}
}

// messageCount returns the current length of the conversation history.
func (a *Agent) messageCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.messages)
}

// Send runs one full turn for a user input: it appends the message, calls the
// model, executes any tool calls, and loops until the model returns text-only
// content or the iteration cap is hit.
//
// Only one turn runs at a time per Agent. If Send is invoked while a previous
// turn is still in flight, the second call returns immediately with a "busy"
// ErrorMsg and does not touch the conversation history. That rejection is the
// one thing that does not end a turn — the first one is still running.
func (a *Agent) Send(ctx context.Context, userText string) {
	if !a.turnMu.TryLock() {
		log.Warn("Send called while another turn is in progress",
			"user_msg_len", len(userText),
		)
		a.publish(ErrorMsg{Err: "agent is still working on the previous turn — please wait for it to finish"})
		return
	}
	defer a.turnMu.Unlock()

	// A turn starts here and ends with exactly one AssistantDoneMsg, on every
	// path out — the UI shows itself as working in between, so a return that
	// skips it leaves the UI waiting on a turn that is already over. Paths that
	// end early say why with an ErrorMsg first; that event reports, it does not
	// end anything.
	turnEnd := AssistantDoneMsg{}
	defer func() { a.publish(turnEnd) }()

	// Send is invoked from a bubbletea Cmd goroutine; without an explicit
	// recover, panics here are swallowed by bubbletea's own catch and the
	// resulting goroutine dump corrupts the TUI screen. Push them at the log
	// file instead, then re-emit as an ErrorMsg so the UI shows something.
	defer func() {
		if r := recover(); r != nil {
			log.Error("Send panic",
				"value", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()),
			)
			a.publish(ErrorMsg{Err: fmt.Sprintf("panic in agent: %v (see logs/)", r)})
		}
	}()

	a.record(llm.Message{Role: llm.RoleUser, Content: userText})
	log.Info("turn start", "user_msg_len", len(userText), "history_msgs", a.messageCount())

	maxIter := a.cfg.Options.MaxToolIterations
	if maxIter <= 0 {
		maxIter = 25
	}

	for iter := 0; iter < maxIter; iter++ {
		if ctx.Err() != nil {
			a.endCancelled(&turnEnd, nil)
			return
		}
		log.Debug("iteration", "n", iter+1, "max", maxIter)
		tools := a.registry.Definitions(a.cfg.Capabilities)
		messages, contextStats := a.requestMessages(tools)
		if contextStats.OverBudget {
			// Safe to abandon the turn here: this is the top of the loop, so
			// every tool call the model made has already been answered and the
			// history it leaves behind is one a later turn can still send.
			log.Error("conversation exceeds the context budget with nothing left to drop",
				"tokens", contextStats.AfterTokens,
				"fixed_tokens", contextStats.FixedTokens,
				"budget", a.cfg.Options.MaxContextTokens,
			)
			a.publish(ErrorMsg{Err: fmt.Sprintf(
				"this turn no longer fits the %d-token context budget (the system prompt and tool definitions alone cost about %d) — /compact it, raise options.max_context_tokens, or start a new session",
				a.cfg.Options.MaxContextTokens, a.calibratedTokens(contextStats.FixedTokens))})
			return
		}
		req := &llm.ChatRequest{
			Model:    a.cfg.Model,
			Messages: messages,
			Tools:    tools,
		}

		// Run one iteration's LLM call, retrying upstream "ghost"
		// responses (no content, no tool calls, zero tokens, no usage
		// chunk) before treating them as turn-end. Those almost always
		// mean OpenRouter / DeepSeek dropped the stream — the model
		// intended to call another tool, so ending the turn would strand
		// the conversation. A legitimate end-of-turn carries usage
		// tokens and is left alone.
		resp, streamed, err := a.callLLMWithRetry(ctx, req, iter+1)
		if err != nil {
			if ctx.Err() != nil {
				// The operator stopped the turn mid-stream. Keep whatever the
				// model had already said: the TUI has shown it, so dropping it
				// would leave the visible transcript claiming something the
				// history — and therefore the next turn's prompt — denies.
				if streamed != "" {
					a.record(llm.Message{Role: llm.RoleAssistant, Content: streamed})
				}
				a.endCancelled(&turnEnd, nil)
				return
			}
			log.Error("LLM call failed", "err", err, "iter", iter+1)
			a.publish(ErrorMsg{Err: fmt.Sprintf("llm error: %v", err)})
			return
		}
		// Record usage into both the session-scoped and run-scoped governors.
		a.gov.Record(resp.Usage)
		a.globGov.Record(resp.Usage)
		if resp.Usage.CostAvailable {
			a.gov.AddCost(resp.Usage.Cost)
			a.globGov.AddCost(resp.Usage.Cost)
		}
		a.calibrate(contextStats.AfterTokens, resp.Usage.InputTokens)
		a.publish(a.statsUpdate(a.promptTokens(contextStats, resp.Usage.InputTokens)))

		// Persist the assistant message with the usage that produced it, so the
		// session's cumulative cost can be restored on resume.
		a.recordUsage(llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}, &session.Usage{
			InputTokens:   resp.Usage.InputTokens,
			OutputTokens:  resp.Usage.OutputTokens,
			Cost:          resp.Usage.Cost,
			CostAvailable: resp.Usage.CostAvailable,
		})

		a.publish(AssistantMessageMsg{})

		// If there are no tool calls, the turn is over.
		if len(resp.ToolCalls) == 0 {
			// If we got here with an empty response after retries
			// exhausted (callLLMWithRetry returned the empty resp),
			// surface a notice so the user knows the model didn't
			// produce anything actionable. Legitimate end-of-turn
			// responses (non-empty content) just log normally.
			if isEmptyGlitch(resp) {
				log.Warn("LLM kept returning empty completions after retries",
					"iter", iter+1)
				a.publish(ErrorMsg{Err: "model returned an empty response — try asking again"})
			} else {
				log.Info("turn done (no tool calls)",
					"iter", iter+1,
					"assistant_len", len(resp.Content),
				)
			}
			return
		}

		log.Info("dispatching tool calls", "iter", iter+1, "count", len(resp.ToolCalls))

		// Execute tool calls and append their results.
		for idx, tc := range resp.ToolCalls {
			if ctx.Err() != nil {
				a.endCancelled(&turnEnd, resp.ToolCalls[idx:])
				return
			}
			a.publish(ToolCallStartMsg{Name: tc.Name, Args: tc.Arguments})
			log.Info("tool call",
				"name", tc.Name,
				"id", tc.ID,
				"args_len", len(tc.Arguments),
			)

			tool, ok := a.registry.Enabled(tc.Name, a.cfg.Capabilities)
			if !ok {
				result := fmt.Sprintf("error: unknown tool %q", tc.Name)
				log.Warn("unknown tool requested", "name", tc.Name, "capabilities", a.cfg.Capabilities)
				a.publish(ToolCallResultMsg{Name: tc.Name, Result: "", Err: result})
				a.appendToolResult(tc.ID, tc.Name, result)
				continue
			}

			if tool.Restricted {
				decision, err := a.gate.Request(ctx, tc.Name, prettyJSON(tc.Arguments))
				if err != nil {
					if ctx.Err() != nil {
						a.endCancelled(&turnEnd, resp.ToolCalls[idx:])
						return
					}
					log.Error("gatekeeper failed", "tool", tc.Name, "err", err)
					a.publish(ErrorMsg{Err: fmt.Sprintf("gatekeeper: %v", err)})
					return
				}
				switch decision {
				case ApprovalApprove:
					// fall through and execute the tool
				case ApprovalDeny:
					log.Info("tool denied by operator", "tool", tc.Name)
					msg := "denied by operator"
					a.publish(ToolCallResultMsg{Name: tc.Name, Err: msg})
					a.appendToolResult(tc.ID, tc.Name, "error: "+msg)
					continue
				case ApprovalCancel:
					log.Info("turn cancelled by operator at tool call", "tool", tc.Name)
					a.publish(ToolCallResultMsg{Name: tc.Name, Err: "cancelled by operator"})
					a.endCancelled(&turnEnd, resp.ToolCalls[idx:])
					return
				}
			}

			toolStart := time.Now()
			result, err := tool.Fn(ctx, tc.Arguments)
			elapsed := time.Since(toolStart)
			if err != nil {
				if ctx.Err() != nil {
					a.publish(ToolCallResultMsg{Name: tc.Name, Err: "cancelled by operator"})
					a.endCancelled(&turnEnd, resp.ToolCalls[idx:])
					return
				}
				log.Error("tool failed",
					"name", tc.Name,
					"err", err,
					"elapsed_ms", elapsed.Milliseconds(),
				)
				var te *executor.TimeoutError
				if errors.As(err, &te) {
					a.publish(TimeoutMsg{Command: te.Command, Message: te.Message})
				}
				a.publish(ToolCallResultMsg{Name: tc.Name, Result: result, Err: err.Error()})
				a.appendToolResult(tc.ID, tc.Name, "error: "+err.Error()+"\n"+result)
				continue
			}
			log.Info("tool ok",
				"name", tc.Name,
				"result_bytes", len(result),
				"elapsed_ms", elapsed.Milliseconds(),
			)
			a.publish(ToolCallResultMsg{Name: tc.Name, Result: result})
			a.appendToolResult(tc.ID, tc.Name, result)
		}
		// Loop back: send tool results to the model for the next turn.
	}

	log.Warn("max tool iterations reached", "max", maxIter)
	a.publish(ErrorMsg{Err: fmt.Sprintf("max tool iterations (%d) reached", maxIter)})
}

// cancelledToolResult answers a tool call the operator stopped before it could
// run. The wording tells the model the stop was deliberate and that the
// operator is about to speak, so it does not re-propose the same call the
// moment it gets another turn.
const cancelledToolResult = "error: cancelled by operator. The user has stopped this turn and will respond directly. Do not retry."

// endCancelled closes out a turn the operator stopped. Every call in unanswered
// gets a synthetic result first: an assistant message whose tool_calls have no
// matching tool replies is rejected by most providers, so leaving one dangling
// would break not just this turn but every turn after it.
//
// It marks the turn's end rather than publishing it: Send emits that on its way
// out, whichever path it leaves by.
func (a *Agent) endCancelled(turnEnd *AssistantDoneMsg, unanswered []llm.ToolCall) {
	for _, tc := range unanswered {
		a.appendToolResult(tc.ID, tc.Name, cancelledToolResult)
	}
	log.Info("turn cancelled", "unanswered_tool_calls", len(unanswered))
	turnEnd.Cancelled = true
}

// maxEmptyLLMRetries is the number of times we'll re-issue the same
// LLM call when the upstream returns an obviously-broken empty stream
// (zero tokens, no content, no tool calls). One extra call usually clears
// transient OpenRouter / provider glitches.
const maxEmptyLLMRetries = 2

// emptyRetryBackoff is the pause between retries. Short enough to not be
// felt by the user as a stall; long enough for an upstream blip to clear.
const emptyRetryBackoff = 500 * time.Millisecond

// isEmptyGlitch identifies an upstream-glitch response: the provider
// ended the stream without emitting content, tool calls, or even a usage
// chunk. A legitimate end-of-turn response always carries token usage,
// even if it has no content/tool_calls.
func isEmptyGlitch(resp *llm.ChatResponse) bool {
	return resp != nil &&
		resp.Content == "" &&
		len(resp.ToolCalls) == 0 &&
		resp.Usage.InputTokens == 0 &&
		resp.Usage.OutputTokens == 0
}

// callLLMWithRetry wraps prov.StreamChat with a small retry on
// empty-glitch responses. Streaming deltas are forwarded as they arrive
// during EACH attempt — if a retry succeeds the user just sees the
// reply land slightly later, no rewind needed. Returns the final
// response (possibly still empty, if all retries glitched) or any
// hard error.
//
// streamed is the content emitted so far, which on an aborted stream is the
// only copy: StreamChat returns no response alongside an error.
func (a *Agent) callLLMWithRetry(ctx context.Context, req *llm.ChatRequest, iter int) (resp *llm.ChatResponse, streamed string, err error) {
	var content strings.Builder
	cb := func(ev llm.StreamEvent) {
		if ev.DeltaText != "" {
			content.WriteString(ev.DeltaText)
			a.publish(AssistantDeltaMsg{Text: ev.DeltaText})
		}
	}

	var lastResp *llm.ChatResponse
	for attempt := 0; attempt <= maxEmptyLLMRetries; attempt++ {
		if attempt > 0 {
			log.Warn("LLM returned empty stream, retrying",
				"iter", iter, "attempt", attempt, "max", maxEmptyLLMRetries)
			select {
			case <-ctx.Done():
				return nil, content.String(), ctx.Err()
			case <-time.After(emptyRetryBackoff):
			}
		}
		resp, err := a.prov.StreamChat(ctx, req, cb)
		if err != nil {
			return nil, content.String(), err
		}
		lastResp = resp
		if !isEmptyGlitch(resp) {
			return resp, content.String(), nil
		}
	}
	// All retries returned empty glitches. Let the caller decide what to
	// do with it; we surface a notice from there.
	return lastResp, content.String(), nil
}

// appendToolResult records a tool response in the conversation history and
// persists it to the session file.
//
// The result is reduced to the configured budget first, and it is the reduced
// form that is both sent and stored. A tool result is not a one-off cost: it is
// replayed to the provider on every later iteration of this turn and every
// later turn in the session, so one unbounded output raises the price of
// everything that follows and eventually pushes the session past the context
// limit for good. Storing the reduced form keeps a resumed transcript honest
// about what the model was actually shown.
func (a *Agent) appendToolResult(toolCallID, name, content string) {
	a.record(llm.Message{
		Role:       llm.RoleTool,
		Content:    a.reduceToolResult(toolCallID, name, content),
		ToolCallID: toolCallID,
		Name:       name,
	})
}

// reduceToolResult normalises a tool result and bounds it to the configured
// budget, spilling the full text to the workspace when anything is dropped so
// the agent can still reach it.
func (a *Agent) reduceToolResult(toolCallID, name, content string) string {
	budget := a.cfg.Options.MaxToolResultBytes
	normalized, stats := toolout.Normalize(content)

	var note string
	if len(normalized) > budget && budget > 0 {
		note = a.spillToolResult(toolCallID, name, normalized)
	}

	reduced, cut := toolout.Truncate(normalized, budget, note)
	stats.TruncatedBytes = cut.TruncatedBytes
	stats.FinalBytes = cut.FinalBytes

	if stats.Removed() > 0 {
		log.Info("tool result reduced",
			"tool", name,
			"original_bytes", stats.OriginalBytes,
			"final_bytes", stats.FinalBytes,
			"ansi_bytes", stats.ANSIBytes,
			"progress_bytes", stats.ProgressBytes,
			"repeat_bytes", stats.RepeatBytes,
			"truncated_bytes", stats.TruncatedBytes,
		)
	}
	return reduced
}

func (a *Agent) snapshotMessages() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]llm.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// prettyJSON reformats raw JSON for display in the approval modal. On parse
// failure it returns the input unchanged.
func prettyJSON(s string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(pretty)
}
