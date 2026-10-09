package tui

import (
	"testing"
	"time"

	"github.com/ruvolof/yaaih/internal/agent"
)

// While the program is busy elsewhere nothing reads the relay; the agent must
// still be able to publish everything it has.
func TestRelayDrainsTheAgentWhileNothingReads(t *testing.T) {
	in := make(chan agent.Event)
	relay := newEventRelay(in)

	sent := make(chan struct{})
	go func() {
		for index := range 1000 {
			in <- agent.AssistantDeltaMsg{Text: string(rune('a' + index%26))}
		}
		in <- agent.ApprovalRequestMsg{}
		close(sent)
	}()

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent blocked publishing while nothing read the relay")
	}
	if !eventually(relay.approvalWaiting) {
		t.Error("approvalWaiting = false with an approval request queued")
	}
	close(in)

	for index := range 1000 {
		event := <-relay.out
		if delta, ok := event.(agent.AssistantDeltaMsg); !ok || delta.Text != string(rune('a'+index%26)) {
			t.Fatalf("event %d = %#v, want the deltas in order", index, event)
		}
	}
	if _, ok := (<-relay.out).(agent.ApprovalRequestMsg); !ok {
		t.Fatal("the approval request did not follow the deltas")
	}
	if !eventually(func() bool { return !relay.approvalWaiting() }) {
		t.Error("approvalWaiting = true once the request was handed on")
	}
	if _, open := <-relay.out; open {
		t.Error("the relay stayed open after the agent's stream closed and everything was handed on")
	}
}

// eventually reports whether condition becomes true within a second: the
// relay's bookkeeping lands just after the hand-off a test can observe.
func eventually(condition func() bool) bool {
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}
