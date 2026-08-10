package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

// connTracker records the server-side lifecycle of every TCP connection an
// httptest server accepts, so a test can assert that the client released them.
type connTracker struct {
	mu     sync.Mutex
	states map[net.Conn]http.ConnState
}

func newConnTracker() *connTracker {
	return &connTracker{states: make(map[net.Conn]http.ConnState)}
}

func (t *connTracker) record(conn net.Conn, state http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.states[conn] = state
}

func (t *connTracker) counts() (accepted, live int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, state := range t.states {
		accepted++
		if state != http.StateClosed && state != http.StateHijacked {
			live++
		}
	}
	return accepted, live
}

// waitForLiveConns polls until the number of connections the server still
// considers open drops to want, and returns the last observed value.
func (t *connTracker) waitForLiveConns(want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		_, live := t.counts()
		if live <= want || time.Now().After(deadline) {
			return live
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sseServer serves keep-alive SSE responses built by the supplied writer. The
// writer receives a hold func that blocks until either the client hangs up or
// the test ends, letting a handler keep a response open the way a provider
// with more of the stream still to send would.
func sseServer(t *testing.T, write func(w http.ResponseWriter, flush func(), hold func())) (*httptest.Server, *connTracker) {
	t.Helper()
	tracker := newConnTracker()
	stall := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write(w, flusher.Flush, func() {
			select {
			case <-stall:
			case <-r.Context().Done():
			}
		})
	}))
	srv.Config.ConnState = tracker.record
	srv.Start()
	t.Cleanup(func() {
		close(stall)
		srv.Close()
	})
	return srv, tracker
}

func chunkEvent(id, content string) string {
	return fmt.Sprintf("data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"model\":\"stub\","+
		"\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", id, content)
}

const usageEvent = `data: {"id":"stub","object":"chat.completion.chunk","model":"stub",` +
	`"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n"

const errorEvent = `data: {"error":{"message":"upstream is rate limited","code":429}}` + "\n\n"

func stubRequest() *ChatRequest {
	return &ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}}
}

// TestStreamChatReleasesConnectionOnStreamError covers the abandoned-stream
// case: the provider reports an error part-way through a response it has not
// finished sending, so the body never reaches EOF on its own. Unless StreamChat
// closes the stream, the socket and its two transport goroutines stay parked
// for the life of the process and every later turn has to dial afresh.
func TestStreamChatReleasesConnectionOnStreamError(t *testing.T) {
	srv, tracker := sseServer(t, func(w http.ResponseWriter, flush func(), hold func()) {
		fmt.Fprint(w, chunkEvent("stub", "partial answer"))
		flush()
		fmt.Fprint(w, errorEvent)
		flush()
		hold()
	})

	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")
	goroutinesBefore := runtime.NumGoroutine()

	const turns = 25
	for turn := 0; turn < turns; turn++ {
		if _, err := provider.StreamChat(context.Background(), stubRequest(), nil); err == nil {
			t.Fatalf("turn %d: expected a stream error", turn)
		}
	}

	accepted, _ := tracker.counts()
	live := tracker.waitForLiveConns(0, 5*time.Second)
	goroutinesAfter := runtime.NumGoroutine()
	t.Logf("after %d failed turns: %d connections accepted, %d still open, goroutines %d -> %d",
		turns, accepted, live, goroutinesBefore, goroutinesAfter)

	if live != 0 {
		t.Errorf("leaked %d of %d connections", live, accepted)
	}
	if grew := goroutinesAfter - goroutinesBefore; grew > turns/2 {
		t.Errorf("goroutines grew by %d over %d failed turns", grew, turns)
	}
}

// TestStreamChatReusesConnectionOnSuccess guards the other half of the fix:
// closing the stream must not cost keep-alive reuse on the happy path.
func TestStreamChatReusesConnectionOnSuccess(t *testing.T) {
	srv, tracker := sseServer(t, func(w http.ResponseWriter, flush func(), _ func()) {
		fmt.Fprint(w, chunkEvent("stub", "hello"))
		flush()
		fmt.Fprint(w, usageEvent)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	})

	provider := NewOpenAICompat(srv.URL, "test-key", "stub-model")
	const turns = 25
	var deltas int
	for turn := 0; turn < turns; turn++ {
		resp, err := provider.StreamChat(context.Background(), stubRequest(), func(ev StreamEvent) {
			if ev.DeltaText != "" {
				deltas++
			}
		})
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if resp.Content != "hello" {
			t.Fatalf("turn %d: content = %q, want %q", turn, resp.Content, "hello")
		}
		if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
			t.Fatalf("turn %d: usage = %+v", turn, resp.Usage)
		}
	}
	if deltas != turns {
		t.Errorf("content deltas = %d, want %d", deltas, turns)
	}

	accepted, _ := tracker.counts()
	t.Logf("after %d successful turns: %d connections accepted", turns, accepted)
	if accepted != 1 {
		t.Errorf("keep-alive reuse broken: %d connections accepted for %d turns", accepted, turns)
	}
}
