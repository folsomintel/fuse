package fuse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Events opens the SSE stream for an environment and returns a channel
// of Event values. The channel is closed when the stream ends: cleanly
// on EOF, or after a final Event whose Err is set on failure. A terminal
// state event (destroyed/failed) is delivered, then the channel closes.
// The stream uses a no-timeout client; cancel ctx to stop it.
func (s *EnvironmentsService) Events(ctx context.Context, vmID string) (<-chan Event, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	path := "/v1/environments/" + url.PathEscape(vmID) + "/events"
	req, err := s.t.newRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	// use the no-timeout streaming client so the long-lived stream is
	// not killed by the default request timeout.
	client := s.t.streamHTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	// CheckResponse closes the body on error, so do not start the
	// goroutine in that case.
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}

	ch := make(chan Event)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()

		// closing the body when ctx is done unblocks the scanner so a
		// canceled context is observed promptly.
		go func() {
			<-ctx.Done()
			_ = resp.Body.Close()
		}()

		scanner := bufio.NewScanner(resp.Body)
		// raise the line cap above the 64k default to handle long data
		// lines.
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

		var data strings.Builder
		hasData := false

		// sendEvent delivers ev respecting ctx cancellation. It reports
		// whether the caller should stop scanning.
		sendEvent := func(ev Event) (stop bool) {
			select {
			case ch <- ev:
				return false
			case <-ctx.Done():
				return true
			}
		}

		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				// blank line terminates the current event.
				if !hasData {
					continue
				}
				var ev Event
				if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
					sendEvent(Event{Err: err})
					return
				}
				data.Reset()
				hasData = false
				if sendEvent(ev) {
					sendEvent(Event{Err: ctx.Err()})
					return
				}
				if IsTerminalState(ev.State) {
					return
				}
			case strings.HasPrefix(line, ":"):
				// comment / keepalive line, skip.
				continue
			case strings.HasPrefix(line, "data:"):
				// strip the prefix and a single optional leading space.
				payload := line[len("data:"):]
				payload = strings.TrimPrefix(payload, " ")
				data.WriteString(payload)
				hasData = true
			default:
				// other fields (id:, event:, ...) are ignored.
				continue
			}
		}

		// scan loop ended: distinguish cancellation, read error, and
		// clean EOF.
		if ctx.Err() != nil {
			sendEvent(Event{Err: ctx.Err()})
			return
		}
		if err := scanner.Err(); err != nil {
			sendEvent(Event{Err: err})
			return
		}
		// clean EOF with no terminal state: just close the channel.
	}()

	return ch, nil
}
