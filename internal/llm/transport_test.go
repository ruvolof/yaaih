package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedServer answers each request with the next status in statuses; a 200
// gets a complete stream. It records the headers of every request it saw.
func scriptedServer(t *testing.T, header http.Header, statuses ...int) (*httptest.Server, func() []http.Header) {
	t.Helper()
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempt := len(seen)
		seen = append(seen, r.Header.Clone())
		mu.Unlock()

		status := http.StatusOK
		if attempt < len(statuses) {
			status = statuses[attempt]
		}
		if status != http.StatusOK {
			for key, values := range header {
				w.Header()[key] = values
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"message":"refused on attempt %d","code":%d}}`, attempt, status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chunkEvent("stub", "ok"))
		fmt.Fprint(w, usageEvent)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), seen...)
	}
}

func immediateRetry() http.Header {
	return http.Header{"Retry-After": []string{"0"}}
}

func TestTransientRefusalsAreRetried(t *testing.T) {
	srv, requests := scriptedServer(t, immediateRetry(), http.StatusTooManyRequests, http.StatusServiceUnavailable)
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	resp, err := provider.StreamChat(context.Background(), stubRequest(), nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q, want %q", resp.Content, "ok")
	}
	if got := len(requests()); got != 3 {
		t.Errorf("requests = %d, want 3 (two refusals, then the stream)", got)
	}
}

func TestRetriesGiveUpWithTheProvidersMessage(t *testing.T) {
	srv, requests := scriptedServer(t, immediateRetry(),
		http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway)
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	_, err := provider.StreamChat(context.Background(), stubRequest(), nil)
	if err == nil {
		t.Fatal("StreamChat succeeded against a provider that only refuses")
	}
	if got := len(requests()); got != 1+maxRequestRetries {
		t.Errorf("requests = %d, want %d", got, 1+maxRequestRetries)
	}
	for _, want := range []string{"502 Bad Gateway", "refused on attempt 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A request the provider rejects as malformed would be rejected again.
func TestClientErrorsAreNotRetried(t *testing.T) {
	srv, requests := scriptedServer(t, immediateRetry(), http.StatusBadRequest)
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	_, err := provider.StreamChat(context.Background(), stubRequest(), nil)
	if err == nil || !strings.Contains(err.Error(), "400 Bad Request") {
		t.Fatalf("error = %v, want the 400 reported", err)
	}
	if got := len(requests()); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// Cancelling a turn has to take effect while a retry is waiting out its delay.
func TestCancelInterruptsTheRetryWait(t *testing.T) {
	slow := http.Header{"Retry-After": []string{"30"}}
	srv, _ := scriptedServer(t, slow, http.StatusTooManyRequests)
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := provider.StreamChat(ctx, stubRequest(), nil)
	if err == nil {
		t.Fatal("StreamChat succeeded after its context expired")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("StreamChat returned after %v, want it to stop when the context did", elapsed)
	}
}

func TestAPIKeyIsSentOnlyWhenConfigured(t *testing.T) {
	srv, requests := scriptedServer(t, nil)

	if _, err := NewOpenAICompat(srv.URL, "test-key", "stub-model").StreamChat(context.Background(), stubRequest(), nil); err != nil {
		t.Fatalf("StreamChat with a key: %v", err)
	}
	if _, err := NewOpenAICompat(srv.URL, "", "stub-model").StreamChat(context.Background(), stubRequest(), nil); err != nil {
		t.Fatalf("StreamChat without a key: %v", err)
	}

	seen := requests()
	if got := seen[0].Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization with a key = %q, want %q", got, "Bearer test-key")
	}
	if got := seen[1].Get("Authorization"); got != "" {
		t.Errorf("Authorization without a key = %q, want none", got)
	}
}

func TestEndpointIsJoinedToTheBaseURL(t *testing.T) {
	for _, baseURL := range []string{"https://example.test/api/v1", "https://example.test/api/v1/"} {
		provider := NewOpenAICompat(baseURL, "", "stub-model")
		if provider.endpoint != "https://example.test/api/v1/chat/completions" {
			t.Errorf("endpoint for %q = %q", baseURL, provider.endpoint)
		}
	}
}

// OpenRouter holds a slow response open with SSE comments, and events may span
// several data lines or arrive with CRLF line ends.
func TestStreamToleratesCommentsAndLineEndings(t *testing.T) {
	srv, _, _ := sseServer(t, func(w http.ResponseWriter, flush func(), _ func()) {
		fmt.Fprint(w, ": OPENROUTER PROCESSING\n\n")
		flush()
		fmt.Fprint(w, strings.ReplaceAll(chunkEvent("stub", "first "), "\n", "\r\n"))
		fmt.Fprint(w, "event: message\n")
		fmt.Fprint(w, "data: {\"id\":\"stub\",\"choices\":[{\"index\":0,\n")
		fmt.Fprint(w, "data: \"delta\":{\"content\":\"second\"}}]}\n\n")
		fmt.Fprint(w, usageEvent)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	})
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	resp, err := provider.StreamChat(context.Background(), stubRequest(), nil)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if resp.Content != "first second" {
		t.Errorf("content = %q, want %q", resp.Content, "first second")
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestMidStreamErrorQuotesTheProvider(t *testing.T) {
	srv, _, _ := sseServer(t, func(w http.ResponseWriter, flush func(), _ func()) {
		fmt.Fprint(w, chunkEvent("stub", "partial"))
		fmt.Fprint(w, errorEvent)
		flush()
	})
	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")

	_, err := provider.StreamChat(context.Background(), stubRequest(), nil)
	if err == nil || !strings.Contains(err.Error(), "upstream is rate limited") {
		t.Errorf("error = %v, want the provider's message", err)
	}
}
