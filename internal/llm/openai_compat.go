// Package llm — OpenAI-compatible provider implementation.
//
// One implementation that targets *any* OpenAI-compatible endpoint by
// configuring BaseURL: DeepSeek, OpenRouter, Ollama (via /v1), LM Studio,
// vLLM, etc. Token usage is read from the API's `usage` field; if a provider
// omits it, the governor falls back to "N/A".
//
// OpenRouter includes a `cost` field (USD) inside the final streamed chunk's
// `usage` block automatically. openai-go doesn't know that field exists, but
// preserves it as raw JSON in ExtraFields; extractOpenRouterCost reads it
// out. See its doc comment for the apijson-specific gotcha around
// Field.IsPresent().
package llm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var log = logging.For("llm")

// OpenAICompat is the single OpenAI-compatible Provider.
type OpenAICompat struct {
	client    openai.Client
	model     string
	name      string
	isOpenRtr bool // true when the base URL points at openrouter.ai — enables cost parsing on responses
}

// NewOpenAICompat constructs a provider against the given base URL with the
// supplied API key (may be empty for local servers).
func NewOpenAICompat(baseURL, apiKey, model string) *OpenAICompat {
	opts := []option.RequestOption{option.WithBaseURL(baseURL)}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	isOR := strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
	log.Info("provider configured",
		"base_url", baseURL,
		"model", model,
		"api_key_set", apiKey != "",
		"openrouter_cost", isOR,
	)
	return &OpenAICompat{
		client:    openai.NewClient(opts...),
		model:     model,
		name:      baseURL,
		isOpenRtr: isOR,
	}
}

// Name returns a human-readable identifier (the base URL).
func (p *OpenAICompat) Name() string { return p.name }

// StreamChat runs a streaming completion and accumulates the final response.
// The callback fires on every content delta. Tool-call deltas are accumulated
// silently and exposed via the returned ChatResponse.
func (p *OpenAICompat) StreamChat(ctx context.Context, req *ChatRequest, cb StreamCallback) (*ChatResponse, error) {
	if cb == nil {
		cb = func(StreamEvent) {}
	}

	params := openai.ChatCompletionNewParams{
		Model:    p.model,
		Messages: toOpenAIMessages(req.Messages),
	}
	if len(req.Tools) > 0 {
		params.Tools = toOpenAITools(req.Tools)
	}

	start := time.Now()
	log.Info("request sent",
		"model", p.model,
		"msgs", len(req.Messages),
		"tools", len(req.Tools),
	)

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)
	// A stream abandoned before its body reaches EOF — the provider erroring
	// part-way through a response — parks the socket and its transport
	// goroutines for the life of the process.
	defer stream.Close()
	acc := openai.ChatCompletionAccumulator{}

	var firstChunkAt time.Time
	chunks := 0
	// lastUsageChunk holds the chunk that carried the final usage block.
	// The accumulator sums the numeric token fields onto its own Usage but
	// doesn't propagate the raw chunk's JSON.ExtraFields, so we hold a
	// reference to the chunk itself for the cost-extraction pass below.
	var lastUsageChunk *openai.ChatCompletionChunk

	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if firstChunkAt.IsZero() {
			firstChunkAt = time.Now()
			log.Info("stream first chunk", "latency_ms", firstChunkAt.Sub(start).Milliseconds())
		}
		chunks++
		if len(chunk.Choices) > 0 {
			if delta := chunk.Choices[0].Delta.Content; delta != "" {
				cb(StreamEvent{DeltaText: delta})
			}
		}
		// Snapshot any chunk that carried token usage. Per OpenRouter docs
		// cost is included in this same block automatically.
		if chunk.Usage.TotalTokens > 0 {
			lastUsageChunk = &chunk
		}
	}
	if err := stream.Err(); err != nil {
		log.Error("stream failed",
			"err", err,
			"chunks_received", chunks,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		return nil, fmt.Errorf("llm: stream: %w", err)
	}
	cb(StreamEvent{Done: true})

	resp := &ChatResponse{
		Usage: TokenUsage{
			InputTokens:  int(acc.Usage.PromptTokens),
			OutputTokens: int(acc.Usage.CompletionTokens),
		},
	}
	if len(acc.Choices) > 0 {
		choice := acc.Choices[0]
		resp.Content = choice.Message.Content
		for _, tc := range choice.Message.ToolCalls {
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	// Extract OpenRouter's per-request cost. On the miss path we dump the
	// raw usage JSON and the unknown-field key sets so it's obvious where
	// the cost field went (or whether the response shape changed).
	if p.isOpenRtr {
		if cost, ok := extractOpenRouterCost(lastUsageChunk); ok {
			resp.Usage.Cost = cost
			resp.Usage.CostAvailable = true
		} else {
			log.Warn("openrouter cost not present in stream",
				"generation_id", acc.ID,
				"had_usage_chunk", lastUsageChunk != nil,
				"chunk_raw", rawJSON(lastUsageChunk),
			)
		}
	}

	logArgs := []any{
		"tokens_in", resp.Usage.InputTokens,
		"tokens_out", resp.Usage.OutputTokens,
		"tool_calls", len(resp.ToolCalls),
		"content_len", len(resp.Content),
		"chunks", chunks,
		"elapsed_ms", time.Since(start).Milliseconds(),
	}
	if resp.Usage.CostAvailable {
		logArgs = append(logArgs, "cost_usd", resp.Usage.Cost)
	}
	log.Info("stream done", logArgs...)

	return resp, nil
}

// extractOpenRouterCost reads the `cost` field out of a streaming chunk's
// Usage.JSON.ExtraFields. OpenRouter adds this field to the OpenAI usage
// schema; openai-go preserves the raw JSON of unknown fields here.
//
// We DO NOT use Field.IsPresent() for this lookup — openai-go's apijson
// decoder marks every extras field as `invalid` (and IsPresent() then
// returns false) when the receiving struct has no typed extras map, which
// is exactly the case for CompletionUsage. The raw JSON value is still
// populated correctly, so we read it via Raw() and parse directly.
func extractOpenRouterCost(chunk *openai.ChatCompletionChunk) (float64, bool) {
	if chunk == nil {
		return 0, false
	}
	raw, ok := chunk.Usage.JSON.ExtraFields["cost"]
	if !ok {
		return 0, false
	}
	s := raw.Raw()
	if s == "" || s == "null" {
		return 0, false
	}
	cost, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Debug("openrouter cost parse failed", "raw", s, "err", err)
		return 0, false
	}
	return cost, true
}

// rawJSON returns the whole chunk as the provider sent it, or "" when the
// chunk is nil. Logged when cost extraction misses: the full chunk covers
// both a renamed field inside `usage` and one that moved to the top level.
func rawJSON(chunk *openai.ChatCompletionChunk) string {
	if chunk == nil {
		return ""
	}
	return chunk.RawJSON()
}

// toOpenAIMessages converts our Message slice to the openai-go union type.
func toOpenAIMessages(msgs []Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case RoleUser:
			out = append(out, openai.UserMessage(m.Content))
		case RoleAssistant:
			am := openai.ChatCompletionAssistantMessageParam{
				Content: openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(m.Content),
				},
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					calls = append(calls, openai.ChatCompletionMessageToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: tc.Arguments,
						},
					})
				}
				am.ToolCalls = calls
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &am})
		case RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		}
	}
	return out
}

// toOpenAITools converts the harness's tool definitions to the openai-go shape.
func toOpenAITools(tools []ToolDefinition) []openai.ChatCompletionToolParam {
	out := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:        t.Name,
				Description: openai.String(t.Description),
				Parameters:  openai.FunctionParameters(t.Parameters),
			},
		})
	}
	return out
}
