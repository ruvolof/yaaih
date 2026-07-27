// Package llm abstracts the LLM provider. Today there is a single
// implementation (OpenAI-compatible) used for DeepSeek, OpenRouter, local
// Ollama/LM Studio, etc. The interface lets us swap in another implementation
// without touching the agent loop.
package llm

import "context"

// Role identifies the speaker of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is the wire-format message exchanged with the model.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // for Role=tool replies
	Name       string     `json:"name,omitempty"`         // tool name on Role=tool replies
}

// ToolDefinition describes a tool the model may call.
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ToolCall is a single tool invocation requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded args, model-shaped
}

// TokenUsage is returned by the API; the governor accumulates it.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int

	// Cost is the USD charge for this turn, when the provider exposes it
	// (OpenRouter includes it on the final usage chunk; OpenAI's direct
	// API does not). Only meaningful when CostAvailable is true.
	Cost          float64
	CostAvailable bool
}

// ChatRequest is one round of input to the model.
type ChatRequest struct {
	Model    string
	Messages []Message
	Tools    []ToolDefinition
}

// ChatResponse aggregates the final accumulated message from a streamed call.
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     TokenUsage
}

// StreamEvent is emitted to the callback during streaming.
type StreamEvent struct {
	// DeltaText is a content chunk; empty if this event is purely a tool-call delta.
	DeltaText string
	// Done is true on the final event (after content + tool calls are finalised).
	Done bool
}

// StreamCallback receives streaming deltas. Implementations should be fast —
// the producer is the same goroutine doing the HTTP read.
type StreamCallback func(StreamEvent)

// Provider is the contract the agent loop depends on.
type Provider interface {
	StreamChat(ctx context.Context, req *ChatRequest, cb StreamCallback) (*ChatResponse, error)
	Name() string
}
