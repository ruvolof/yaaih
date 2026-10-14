// Package llm — OpenAI-compatible provider implementation.
//
// One implementation that targets *any* OpenAI-compatible endpoint by
// configuring BaseURL: DeepSeek, OpenRouter, Ollama (via /v1), LM Studio,
// vLLM, etc. It speaks the streaming Chat Completions protocol directly: a
// JSON POST to <base>/chat/completions answered by server-sent events, one
// chunk per event, ending with `data: [DONE]`. Token usage is read from the
// chunk carrying a `usage` block; if a provider omits it, the governor falls
// back to "N/A".
//
// OpenRouter adds a `cost` field (USD) to that same usage block.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ruvolof/yaaih/internal/logging"
)

var log = logging.For("llm")

const (
	// maxRequestRetries is how many times a request the provider refused or
	// never answered is sent again. Once the stream has started nothing is
	// retried: deltas have already reached the operator.
	maxRequestRetries = 2
	maxRetryBackoff   = 8 * time.Second
	// maxErrorBodyBytes bounds how much of an error response is quoted back.
	maxErrorBodyBytes = 4 << 10
)

// OpenAICompat is the single OpenAI-compatible Provider.
type OpenAICompat struct {
	httpClient  *http.Client
	endpoint    string
	endpointErr error
	apiKey      string
	model       string
	name        string
	isOpenRtr   bool // true when the base URL points at openrouter.ai — enables cost parsing on responses
}

// NewOpenAICompat constructs a provider against the given base URL with the
// supplied API key (may be empty for local servers).
func NewOpenAICompat(baseURL, apiKey, model string) *OpenAICompat {
	endpoint, endpointErr := url.JoinPath(baseURL, "chat/completions")
	isOR := strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
	log.Info("provider configured",
		"base_url", baseURL,
		"model", model,
		"api_key_set", apiKey != "",
		"openrouter_cost", isOR,
	)
	return &OpenAICompat{
		httpClient:  &http.Client{},
		endpoint:    endpoint,
		endpointErr: endpointErr,
		apiKey:      apiKey,
		model:       model,
		name:        baseURL,
		isOpenRtr:   isOR,
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
	if p.endpointErr != nil {
		return nil, fmt.Errorf("llm: base url: %w", p.endpointErr)
	}
	body, err := json.Marshal(p.wireRequest(req))
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}

	start := time.Now()
	log.Info("request sent",
		"model", p.model,
		"msgs", len(req.Messages),
		"tools", len(req.Tools),
	)

	state := &streamState{}
	resp, err := p.receive(ctx, body, state, cb)
	if err != nil {
		log.Error("stream failed",
			"err", err,
			"chunks_received", state.chunks,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		return nil, err
	}
	cb(StreamEvent{Done: true})

	if p.isOpenRtr {
		if state.usage != nil && state.usage.Cost != nil {
			resp.Usage.Cost = *state.usage.Cost
			resp.Usage.CostAvailable = true
		} else {
			log.Warn("openrouter cost not present in stream",
				"generation_id", state.id,
				"had_usage_chunk", state.usage != nil,
				"chunk_raw", state.usageChunkRaw,
			)
		}
	}

	logArgs := []any{
		"tokens_in", resp.Usage.InputTokens,
		"tokens_out", resp.Usage.OutputTokens,
		"tool_calls", len(resp.ToolCalls),
		"content_len", len(resp.Content),
		"chunks", state.chunks,
		"elapsed_ms", time.Since(start).Milliseconds(),
	}
	if resp.Usage.CostAvailable {
		logArgs = append(logArgs, "cost_usd", resp.Usage.Cost)
	}
	log.Info("stream done", logArgs...)

	return resp, nil
}

// receive sends the request and reads the event stream into state, forwarding
// content deltas to cb as they arrive.
func (p *OpenAICompat) receive(ctx context.Context, body []byte, state *streamState, cb StreamCallback) (*ChatResponse, error) {
	httpResp, err := p.send(ctx, body)
	if err != nil {
		return nil, err
	}
	// A body closed before EOF costs the connection, which is what an
	// abandoned stream has to give up: the provider may never finish it.
	defer httpResp.Body.Close()

	events := sseReader{lines: bufio.NewReader(httpResp.Body)}
	sawDone := false
	for {
		data, err := events.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("llm: stream: %w", err)
		}
		// Reading on to EOF after [DONE] is what lets the transport put the
		// connection back for the next turn.
		if sawDone {
			continue
		}
		if bytes.HasPrefix(data, []byte("[DONE]")) {
			sawDone = true
			continue
		}
		if err := state.add(data, cb); err != nil {
			return nil, fmt.Errorf("llm: stream: %w", err)
		}
	}
	return state.response(), nil
}

// send POSTs the request, retrying a refusal or failure that might be
// transient. The returned response has a 2xx status and a body to stream.
func (p *OpenAICompat) send(ctx context.Context, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("llm: build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if p.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
		}

		httpResp, err := p.httpClient.Do(httpReq)
		if ctx.Err() != nil {
			if httpResp != nil {
				httpResp.Body.Close()
			}
			return nil, fmt.Errorf("llm: %w", ctx.Err())
		}

		var failure error
		var responseHeader http.Header
		switch {
		case err != nil:
			failure = fmt.Errorf("llm: POST %s: %w", p.endpoint, err)
		case httpResp.StatusCode >= 200 && httpResp.StatusCode < 300:
			return httpResp, nil
		default:
			failure = statusError(p.endpoint, httpResp)
			responseHeader = httpResp.Header
			if !retryableStatus(httpResp.StatusCode) {
				return nil, failure
			}
		}
		if attempt >= maxRequestRetries {
			return nil, failure
		}

		delay := retryDelay(responseHeader, attempt)
		log.Warn("request failed, retrying", "err", failure, "attempt", attempt+1, "delay_ms", delay.Milliseconds())
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("llm: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
}

// statusError reads a refused response into an error quoting what the
// provider said, and closes it.
func statusError(endpoint string, httpResp *http.Response) error {
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBodyBytes))
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Error) > 0 {
		raw = envelope.Error
	}
	return fmt.Errorf("llm: POST %s: %s %s", endpoint, httpResp.Status, bytes.TrimSpace(raw))
}

func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusConflict ||
		code == http.StatusTooManyRequests ||
		code >= http.StatusInternalServerError
}

// retryDelay honours a Retry-After under a minute and otherwise backs off
// exponentially from half a second, with jitter so that sessions refused
// together do not come back together.
func retryDelay(header http.Header, attempt int) time.Duration {
	if value := header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 && seconds < 60 {
			return time.Duration(seconds * float64(time.Second))
		}
		if when, err := http.ParseTime(value); err == nil {
			if wait := time.Until(when); wait >= 0 && wait < time.Minute {
				return wait
			}
		}
	}
	backoff := min(time.Second/2<<attempt, maxRetryBackoff)
	return backoff - rand.N(backoff/4)
}

// sseReader yields the data of each event in a server-sent event stream.
type sseReader struct {
	lines *bufio.Reader
	data  bytes.Buffer
}

// next returns the data of the next event, valid until the following call,
// or io.EOF once the stream ends. Comments (a provider's keep-alives) and
// fields other than data are skipped, and an event the stream ends in the
// middle of is dropped.
func (r *sseReader) next() ([]byte, error) {
	r.data.Reset()
	for {
		line, err := r.lines.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if r.data.Len() > 0 {
				return r.data.Bytes(), nil
			}
			continue
		}
		if field, value, _ := bytes.Cut(line, []byte(":")); string(field) == "data" {
			if r.data.Len() > 0 {
				r.data.WriteByte('\n')
			}
			r.data.Write(bytes.TrimPrefix(value, []byte(" ")))
		}
	}
}

// wireChunk is the part of a streamed chat.completion.chunk the harness reads.
type wireChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *wireUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type wireUsage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	Cost             *float64 `json:"cost"`
}

// streamState accumulates the chunks of one response.
type streamState struct {
	id            string
	chunks        int
	content       strings.Builder
	toolCalls     []ToolCall
	usage         *wireUsage
	usageChunkRaw string
}

func (s *streamState) add(data []byte, cb StreamCallback) error {
	var chunk wireChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("decode chunk: %w", err)
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		return fmt.Errorf("provider error mid-stream: %s", chunk.Error)
	}
	if s.chunks == 0 {
		s.id = chunk.ID
	}
	s.chunks++

	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		if choice.Delta.Content != "" {
			s.content.WriteString(choice.Delta.Content)
			cb(StreamEvent{DeltaText: choice.Delta.Content})
		}
		for _, delta := range choice.Delta.ToolCalls {
			// Providers number calls from 0 without gaps. Any other index
			// starts a new call rather than sizing the slice to fit it.
			index := delta.Index
			if index < 0 || index > len(s.toolCalls) {
				index = len(s.toolCalls)
			}
			if index == len(s.toolCalls) {
				s.toolCalls = append(s.toolCalls, ToolCall{})
			}
			call := &s.toolCalls[index]
			if delta.ID != "" {
				call.ID = delta.ID
			}
			call.Name += delta.Function.Name
			call.Arguments += delta.Function.Arguments
		}
	}

	// The spec puts usage on the final chunk only, but some providers repeat
	// a running total on every chunk, so the last one wins rather than a sum.
	if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
		s.usage = chunk.Usage
		s.usageChunkRaw = string(data)
	}
	return nil
}

func (s *streamState) response() *ChatResponse {
	resp := &ChatResponse{
		Content:   s.content.String(),
		ToolCalls: s.toolCalls,
	}
	if s.usage != nil {
		resp.Usage.InputTokens = s.usage.PromptTokens
		resp.Usage.OutputTokens = s.usage.CompletionTokens
	}
	return resp
}

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

type wireMessage struct {
	Role       Role           `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string             `json:"type"`
	Function wireToolDefinition `json:"function"`
}

type wireToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

func (p *OpenAICompat) wireRequest(req *ChatRequest) wireRequest {
	wire := wireRequest{
		Model:    p.model,
		Messages: make([]wireMessage, 0, len(req.Messages)),
		Stream:   true,
	}
	for _, message := range req.Messages {
		converted := wireMessage{Role: message.Role, Content: message.Content}
		switch message.Role {
		case RoleAssistant:
			for _, call := range message.ToolCalls {
				converted.ToolCalls = append(converted.ToolCalls, wireToolCall{
					ID:       call.ID,
					Type:     "function",
					Function: wireFunction{Name: call.Name, Arguments: call.Arguments},
				})
			}
		case RoleTool:
			converted.ToolCallID = message.ToolCallID
		}
		wire.Messages = append(wire.Messages, converted)
	}
	for _, tool := range req.Tools {
		wire.Tools = append(wire.Tools, wireTool{
			Type: "function",
			Function: wireToolDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}
	return wire
}
