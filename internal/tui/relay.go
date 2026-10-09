package tui

import (
	"sync"

	"github.com/ruvolof/yaaih/internal/agent"
)

// eventRelay drains the agent's event stream as fast as the agent produces it
// and hands the events on as fast as the program takes them. The agent's own
// buffer is small, and the program stops taking events altogether while it has
// handed the terminal to the operator's shell; without the relay, a turn
// streaming meanwhile would stall mid-reply once that buffer filled.
type eventRelay struct {
	out chan agent.Event

	mu      sync.Mutex
	queue   []agent.Event
	pending chan struct{} // signalled when the queue gains an event
	// approvals counts queued approval requests: an agent stopped at one
	// waits for the operator, which the operator's shell announces.
	approvals int
	// approvalQueued is signalled whenever an approval request is queued.
	approvalQueued chan struct{}
}

func newEventRelay(in <-chan agent.Event) *eventRelay {
	relay := &eventRelay{
		out:            make(chan agent.Event),
		pending:        make(chan struct{}, 1),
		approvalQueued: make(chan struct{}, 1),
	}
	go relay.receive(in)
	return relay
}

// receive queues every event from in, and closes the queue's end once in is
// closed and everything queued has been handed on.
func (r *eventRelay) receive(in <-chan agent.Event) {
	done := make(chan struct{})
	go r.deliver(done)
	for event := range in {
		r.mu.Lock()
		r.queue = append(r.queue, event)
		if _, approval := event.(agent.ApprovalRequestMsg); approval {
			r.approvals++
			notify(r.approvalQueued)
		}
		r.mu.Unlock()
		notify(r.pending)
	}
	close(done)
	notify(r.pending)
}

func (r *eventRelay) deliver(inputClosed <-chan struct{}) {
	defer close(r.out)
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.mu.Unlock()
			select {
			case <-r.pending:
				continue
			case <-inputClosed:
				r.mu.Lock()
				empty := len(r.queue) == 0
				r.mu.Unlock()
				if empty {
					return
				}
				continue
			}
		}
		event := r.queue[0]
		r.mu.Unlock()

		r.out <- event

		r.mu.Lock()
		r.queue = r.queue[1:]
		if _, approval := event.(agent.ApprovalRequestMsg); approval {
			r.approvals--
		}
		r.mu.Unlock()
	}
}

// approvalWaiting reports whether an approval request is queued but not yet
// shown to the operator.
func (r *eventRelay) approvalWaiting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.approvals > 0
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
