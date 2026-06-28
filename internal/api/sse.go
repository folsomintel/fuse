package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/go-chi/chi/v5"
)

// sseKeepaliveInterval is the cadence at which the handler emits a
// `: keepalive\n\n` comment line. SSE comment lines are no-ops on the
// client side but keep proxies (load balancers, reverse proxies, NAT
// timeouts, etc.) from idling-out the connection. 15s is conservative
// — most LBs and proxies time out at 30-60s, so 15s gives us a 2-4×
// safety margin.
const sseKeepaliveInterval = 15 * time.Second

// streamEnvironmentEvents serves an SSE stream of state-change events
// for a single VM. The control-server uses this in place of polling
// GET /v1/environments/{vmId} every 2s during provisioning.
//
// Endpoint contract (also documented in openapi.yaml):
//
//	GET /v1/environments/{vmId}/events
//	Accept: text/event-stream
//
// Response headers:
//
//	Content-Type: text/event-stream
//	Cache-Control: no-cache
//	X-Accel-Buffering: no   (disables nginx response buffering)
//
// Behaviour:
//
//   - 404 if the VM is not tracked.
//   - 500 if the response writer does not support flushing (should
//     never happen with net/http; included for safety).
//   - First event sent is a snapshot of the current state so the
//     client doesn't have to GET first to know where it stands.
//   - Subsequent events fire on every state transition (provisioning
//     → running → draining → destroying → destroyed/failed).
//   - Stream closes cleanly on terminal state ("destroyed", "failed").
//   - Stream closes cleanly on client disconnect (request context
//     cancellation).
//   - Heartbeat: a `: keepalive\n\n` comment is sent every 15s to
//     keep idle proxies from closing the connection.
//
// This is a single-process pub/sub: subscribers connected to a
// different orchestrator replica than the publishing replica will
// not see events. The orchestrator runs as a single process today
// so this is acceptable.
//
// WriteTimeout: the orchestrator's http.Server has a global
// WriteTimeout that would terminate long-lived SSE connections. We
// disable the deadline for this request only via
// http.NewResponseController so the rest of the API keeps its
// timeout protection. If the underlying connection does not support
// SetWriteDeadline (e.g. a custom test transport) the call returns
// http.ErrNotSupported, which we treat as non-fatal — the handler
// will simply rely on the request context for cancellation.
//
//	@Summary		Stream environment state events
//	@Description	Server-Sent Events stream of state-change events for a VM. Replaces polling GET /v1/environments/{vmId}.
//	@Tags			environments
//	@Produce		text/event-stream
//	@Param			vmId			path	string	true	"VM identifier"
//	@Param			last_event_id	query	string	false	"Resume cursor from a previous event id (ignored in v1 — included for forward compatibility)"
//	@Success		200				"SSE stream"
//	@Failure		404				{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/environments/{vmId}/events [get]
func (h *Handler) streamEnvironmentEvents(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")

	// Verify the VM exists before flipping into streaming mode.
	// Returning a JSON 404 body here is only possible while we
	// still control Content-Type; once we set text/event-stream the
	// browser/EventSource will refuse to interpret JSON errors.
	info, ok := h.Fleet.GetVM(vmID)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "vm "+vmID+" not found", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "streaming unsupported by transport", nil)
		return
	}

	// Subscribe BEFORE sending the snapshot so we don't miss events
	// that fire between snapshot and the first channel read. The
	// broadcaster's bounded buffer absorbs the brief overlap.
	sub, cancel := h.Fleet.SubscribeEnvironmentEvents(vmID)
	defer cancel()

	// Disable the server-wide WriteTimeout for this connection.
	// http.NewResponseController is the supported way to do this in
	// Go 1.20+. On transports that don't implement SetWriteDeadline
	// (rare; httptest does), we silently ignore the error and rely
	// on r.Context() cancellation to bound the handler lifetime.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && err != http.ErrNotSupported {
		// Don't bail — the handler can still function, the
		// connection just inherits the global WriteTimeout.
		// Logging is intentionally absent here because tests would
		// otherwise spam stderr; an operator who hits this in
		// production will see SSE streams die at WriteTimeout
		// boundaries and can investigate then.
		_ = err
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering is an nginx-specific header; harmless on
	// other proxies. Without it, nginx will buffer the SSE response
	// and the client sees nothing until the connection closes.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Snapshot first. The synthetic event id is fresh so resume
	// clients can use it as a cursor on reconnect.
	snapshot := orchestrator.EnvironmentEvent{
		ID:        snapshotEventID(),
		Kind:      "state",
		VMID:      info.ID,
		State:     info.State,
		URL:       info.URL,
		Error:     info.Error,
		UpdatedAt: info.UpdatedAt,
	}
	if !writeSSEEvent(w, flusher, snapshot) {
		return
	}
	if orchestrator.IsTerminalState(snapshot.State) {
		// VM is already in a terminal state — nothing more to do.
		return
	}

	ticker := time.NewTicker(sseKeepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub:
			if !ok {
				// Broadcaster closed our channel (cancel called
				// from another goroutine, or shutdown). Treat as
				// normal end of stream.
				return
			}
			if !writeSSEEvent(w, flusher, ev) {
				return
			}
			if orchestrator.IsTerminalState(ev.State) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			// Reset the write deadline on every keepalive in case
			// the underlying transport reasserts it (some proxies
			// do). Errors are non-fatal.
			_ = rc.SetWriteDeadline(time.Time{})
		}
	}
}

// writeSSEEvent serialises ev as an SSE message and flushes. Returns
// false if the connection is broken so the caller can stop.
//
// SSE wire format reminder (one event):
//
//	id: <event-uuid>
//	data: {"event":"state",...}
//	\n
//
// We deliberately omit `event:` (the event-name field) because all
// events on this stream are state events for v1; clients listen on
// the default `message` channel of EventSource. Adding an event-name
// would require clients to addEventListener() — extra ceremony for
// no benefit while we have only one event kind.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, ev orchestrator.EnvironmentEvent) bool {
	payload, err := json.Marshal(ev)
	if err != nil {
		// Marshal failure on a struct with only primitive fields
		// is unreachable in practice, but if it happens the safest
		// thing is to silently drop the event rather than tear
		// down the connection.
		return true
	}
	if ev.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", ev.ID); err != nil {
			return false
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// snapshotEventID returns a fresh event id for the synthesised
// initial snapshot. We use the orchestrator package's ID generator
// indirectly via a tiny adapter so the api package doesn't have to
// import crypto/rand directly.
func snapshotEventID() string {
	return orchestrator.NewEventID()
}
