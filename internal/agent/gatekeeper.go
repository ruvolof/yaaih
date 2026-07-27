// Gatekeeper mediates restricted tool execution. The agent loop calls
// Request(...) before invoking a restricted tool; if approval is required,
// the request is forwarded over a channel for the TUI to render and the call
// blocks until the operator answers.
package agent

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var gateLog = logging.For("gatekeeper")

// ApprovalDecision is the operator's response to a restricted tool call.
type ApprovalDecision int

const (
	// ApprovalApprove allows the tool call to proceed.
	ApprovalApprove ApprovalDecision = iota
	// ApprovalDeny refuses this single call; the agent records a "denied"
	// result and continues the turn, so the model may try a different
	// approach on the next iteration.
	ApprovalDeny
	// ApprovalCancel refuses the call AND ends the current turn — the
	// agent stops iterating and hands control back to the operator. Use
	// this to break out of a chain where the model keeps proposing
	// alternatives you don't want.
	ApprovalCancel
)

// ApprovalRequest describes a pending tool call awaiting operator confirmation.
type ApprovalRequest struct {
	ID       uint64
	ToolName string
	Args     string // pretty-printed JSON
	respond  chan ApprovalDecision
}

// Approve marks the request as approved (tool runs).
func (r *ApprovalRequest) Approve() {
	gateLog.Info("approved", "id", r.ID, "tool", r.ToolName)
	r.respond <- ApprovalApprove
	close(r.respond)
}

// Deny refuses this single tool call; the agent's loop will continue and
// the model is told the call was denied.
func (r *ApprovalRequest) Deny() {
	gateLog.Info("denied", "id", r.ID, "tool", r.ToolName)
	r.respond <- ApprovalDeny
	close(r.respond)
}

// Cancel refuses the tool call and signals that the entire turn should end —
// control returns to the operator without the model getting another iteration.
func (r *ApprovalRequest) Cancel() {
	gateLog.Info("turn cancelled", "id", r.ID, "tool", r.ToolName)
	r.respond <- ApprovalCancel
	close(r.respond)
}

// Gatekeeper is safe for concurrent use.
type Gatekeeper struct {
	required bool
	requests chan *ApprovalRequest

	nextID uint64

	mu        sync.Mutex
	autoYes   bool // session-wide override (e.g. operator pressed "a" for all)
	closeOnce sync.Once
}

// NewGatekeeper returns a Gatekeeper configured by the agent's safety policy.
func NewGatekeeper(approvalRequired bool) *Gatekeeper {
	return &Gatekeeper{
		required: approvalRequired,
		requests: make(chan *ApprovalRequest, 4),
	}
}

// Close stops the request stream. The forwarder goroutine ranging over
// Requests() exits once the channel is closed, so it does not leak when an
// agent is torn down (e.g. switching agents). Safe to call multiple times;
// must only be called once no further Request calls can be in flight (the
// agent guarantees this — sessions are switched only when no turn is running).
func (g *Gatekeeper) Close() {
	g.closeOnce.Do(func() { close(g.requests) })
}

// Requests returns the channel the TUI reads to render approval prompts.
func (g *Gatekeeper) Requests() <-chan *ApprovalRequest { return g.requests }

// SetAutoApprove flips the session-wide override. Once set, restricted calls
// no longer prompt for the remainder of the session.
func (g *Gatekeeper) SetAutoApprove(v bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.autoYes = v
}

// Request blocks until the operator decides. When approval is not required
// (or auto-approve is on), returns ApprovalApprove immediately without
// emitting a request.
func (g *Gatekeeper) Request(ctx context.Context, toolName, args string) (ApprovalDecision, error) {
	g.mu.Lock()
	auto := g.autoYes
	g.mu.Unlock()
	if !g.required || auto {
		return ApprovalApprove, nil
	}

	id := atomic.AddUint64(&g.nextID, 1)
	req := &ApprovalRequest{
		ID:       id,
		ToolName: toolName,
		Args:     args,
		respond:  make(chan ApprovalDecision, 1),
	}
	gateLog.Info("approval requested", "id", id, "tool", toolName, "args_len", len(args))
	select {
	case g.requests <- req:
	case <-ctx.Done():
		return ApprovalDeny, ctx.Err()
	}
	select {
	case d := <-req.respond:
		return d, nil
	case <-ctx.Done():
		return ApprovalDeny, ctx.Err()
	}
}
