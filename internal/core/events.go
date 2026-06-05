package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

// EnvironmentEvent is a single state-change notification published by
// the FleetManager broadcaster and consumed by SSE subscribers.
//
// The struct is deliberately a flat, JSON-serialisable shape so the
// REST handler can encode it directly into an SSE `data:` line. The
// `Kind` field always serialises as `event` (matching the SSE event
// dispatch contract on the client) — for v1 the only kind emitted is
// "state". Future event kinds (e.g. "log", "snapshot") would be added
// here without breaking existing subscribers.
//
// Wire shape (one line of SSE data:):
//
//	{
//	  "id": "<event-uuid>",
//	  "event": "state",
//	  "vm_id": "...",
//	  "state": "running",
//	  "url": "host:port",
//	  "error": "...",
//	  "updated_at": "..."
//	}
type EnvironmentEvent struct {
	ID        string    `json:"id"`
	Kind      string    `json:"event"`
	VMID      string    `json:"vm_id"`
	State     VMState   `json:"state"`
	URL       string    `json:"url,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VMStateDestroyed is a synthetic terminal state emitted over the wire
// when a VM is removed from the fleet map (either via successful
// destroy or reap). It is NOT stored as a VMState on a vm record —
// once the vm is gone from the in-memory map there is nothing left to
// have a state — but subscribers need a terminal signal so they can
// close their stream cleanly.
const (
	VMStateDestroyed VMState = "destroyed"
	VMStateFailed    VMState = "failed"
)

// IsTerminalState reports whether the given VM state is a terminal
// state from the SSE subscriber's perspective. After a terminal event
// the broadcaster will not publish further events for the same VM, so
// the handler closes the stream.
func IsTerminalState(s VMState) bool {
	switch s {
	case VMStateDestroyed, VMStateFailed:
		return true
	}
	return false
}

// eventBroadcaster is a single-process, in-memory pub/sub keyed by VM
// ID. It is intentionally minimal — it does NOT survive process
// restart and does NOT replicate across orchestrator replicas. SSE
// subscribers connected to a different replica than the publishing
// replica will simply not see events. Callers that need cross-replica
// fanout should layer Redis pub/sub or NATS on top (out of scope for
// v1: today only one orchestrator runs in production).
//
// Each subscriber gets its own bounded channel. Slow subscribers
// (channel full) drop events and a warning is logged. Dropping is
// preferable to blocking publishers (which would back-pressure the
// FleetManager critical section) or to an unbounded buffer (memory
// leak risk under sustained load).
type eventBroadcaster struct {
	mu     sync.RWMutex
	nextID atomic.Uint64

	// subs maps vmID -> set of subscribers. Each subscriber has its
	// own channel so unsubscribes only need to delete one map entry
	// rather than walk a slice.
	subs map[string]map[*subscriber]struct{}
}

type subscriber struct {
	ch chan EnvironmentEvent
}

// subscriberBufferSize bounds per-subscriber buffering. 32 is enough
// to absorb a short burst of events (e.g. provisioning → running) for
// a slow client; beyond that, events are dropped (logged).
const subscriberBufferSize = 32

func newEventBroadcaster() *eventBroadcaster {
	return &eventBroadcaster{
		subs: make(map[string]map[*subscriber]struct{}),
	}
}

// subscribe registers a new subscriber for the given VM ID. The
// returned channel receives events until cancel is called or the
// broadcaster drops the connection. cancel is idempotent and safe to
// call from any goroutine.
func (b *eventBroadcaster) subscribe(vmID string) (<-chan EnvironmentEvent, func()) {
	s := &subscriber{ch: make(chan EnvironmentEvent, subscriberBufferSize)}

	b.mu.Lock()
	if _, ok := b.subs[vmID]; !ok {
		b.subs[vmID] = make(map[*subscriber]struct{})
	}
	b.subs[vmID][s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if subs, ok := b.subs[vmID]; ok {
				if _, present := subs[s]; present {
					delete(subs, s)
					close(s.ch)
				}
				if len(subs) == 0 {
					delete(b.subs, vmID)
				}
			}
			b.mu.Unlock()
		})
	}
	return s.ch, cancel
}

// publish dispatches an event to all subscribers of the VM. Slow
// subscribers (channel full) are skipped; the dropped event count is
// returned so callers can log it if they want (the FleetManager does).
func (b *eventBroadcaster) publish(ev EnvironmentEvent) (delivered, dropped int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	subs, ok := b.subs[ev.VMID]
	if !ok {
		return 0, 0
	}
	for s := range subs {
		select {
		case s.ch <- ev:
			delivered++
		default:
			dropped++
		}
	}
	return delivered, dropped
}

// subscriberCount returns the number of active subscribers for a VM.
// Used by tests and metrics; not part of the public Fleet surface.
func (b *eventBroadcaster) subscriberCount(vmID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[vmID])
}

// NewEventID returns a 128-bit hex-encoded random identifier suitable
// for use as an SSE `id:` field. Using crypto/rand avoids pulling in
// a UUID dependency and gives the same uniqueness guarantee. The
// stringified form is opaque to clients — they round-trip it via
// Last-Event-ID for resume.
//
// Exported so the api package can mint ids for synthesised snapshot
// events without importing crypto/rand directly.
func NewEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively unrecoverable. Return
		// empty so the SSE handler falls back to omitting the id:
		// line rather than emitting a colliding placeholder.
		return ""
	}
	return hex.EncodeToString(b[:])
}
