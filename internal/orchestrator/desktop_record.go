package orchestrator

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

// a recorded desktop takeover is an input-action log plus periodic
// screenshots. the log is read off the viewer-to-guest half of the rfb
// stream (pointer clicks, keys, clipboard pastes); the guest-to-viewer half
// is framebuffer bytes and is not parsed. like the terminal cast, which
// keeps output and never keystrokes, the log never holds what a human
// typed: printable keys are counted, and only named keys (Return, Tab,
// ctrl+c, ...) are written down. the screenshots are what shows the work.

const (
	// desktopShotInterval is how often a screenshot is taken while a human
	// holds the desktop.
	desktopShotInterval = 10 * time.Second
	// desktopShotTimeout bounds one screenshot.
	desktopShotTimeout = 15 * time.Second
	// maxDesktopRecordingBytes caps one session; past it screenshots stop
	// and the recording is marked truncated. the view keeps working.
	maxDesktopRecordingBytes = 32 << 20
	// maxDesktopEvents caps the input log the same way.
	maxDesktopEvents = 50000
)

// desktopEvent is one entry of the input-action log. T is seconds since
// the session started.
type desktopEvent struct {
	T       float64 `json:"t"`
	Type    string  `json:"type"`
	X       *int    `json:"x,omitempty"`
	Y       *int    `json:"y,omitempty"`
	Buttons *int    `json:"buttons,omitempty"`
	Key     string  `json:"key,omitempty"`
	Chars   int     `json:"chars,omitempty"`
	Bytes   int     `json:"bytes,omitempty"`
}

// desktopShot is one screenshot, a base64 png.
type desktopShot struct {
	T   float64 `json:"t"`
	PNG string  `json:"png"`
}

// desktopRecording is the stored form of a desktop session.
type desktopRecording struct {
	Version        int       `json:"version"`
	Kind           string    `json:"kind"`
	InterventionID string    `json:"intervention_id"`
	EnvID          string    `json:"env_id"`
	Actor          string    `json:"actor"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at"`
	Truncated      bool      `json:"truncated,omitempty"`
	// InputLogIncomplete is set when the viewer's stream stopped parsing
	// (an rfb extension the log does not know), after which no more input
	// was logged. the screenshots carry on.
	InputLogIncomplete bool           `json:"input_log_incomplete,omitempty"`
	Events             []desktopEvent `json:"events"`
	Screenshots        []desktopShot  `json:"screenshots"`
}

// desktopRecorder is a desktop stream with a recorder on it.
type desktopRecorder struct {
	io.ReadWriteCloser
	mu    sync.Mutex
	rec   desktopRecording
	size  int
	input *rfbInput
	stop  chan struct{}
	wg    sync.WaitGroup
	once  sync.Once
	done  func(desktopRecording)
}

// newDesktopRecorder wraps a desktop stream. shoot takes one screenshot; it
// runs at once and then every desktopShotInterval until Close.
func newDesktopRecorder(s io.ReadWriteCloser, rec desktopRecording, interval time.Duration, shoot func(context.Context) (string, error), done func(desktopRecording)) *desktopRecorder {
	rec.Version, rec.Kind = 1, recordingDesktop
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now()
	}
	rec.Events, rec.Screenshots = []desktopEvent{}, []desktopShot{}
	r := &desktopRecorder{ReadWriteCloser: s, rec: rec, stop: make(chan struct{}), done: done}
	r.input = &rfbInput{onEvent: r.event}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			r.screenshot(shoot)
			select {
			case <-r.stop:
				return
			case <-t.C:
			}
		}
	}()
	return r
}

func (r *desktopRecorder) since() float64 { return time.Since(r.rec.StartedAt).Seconds() }

func (r *desktopRecorder) event(e desktopEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rec.Events) >= maxDesktopEvents {
		r.rec.Truncated = true
		return
	}
	e.T = r.since()
	// consecutive typing is one entry.
	if n := len(r.rec.Events); e.Type == "text" && n > 0 && r.rec.Events[n-1].Type == "text" {
		r.rec.Events[n-1].Chars += e.Chars
		return
	}
	r.rec.Events = append(r.rec.Events, e)
}

func (r *desktopRecorder) screenshot(shoot func(context.Context) (string, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), desktopShotTimeout)
	defer cancel()
	png, err := shoot(ctx)
	if err != nil || png == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+len(png) > maxDesktopRecordingBytes {
		r.rec.Truncated = true
		return
	}
	r.size += len(png)
	r.rec.Screenshots = append(r.rec.Screenshots, desktopShot{T: r.since(), PNG: png})
}

// Write is the viewer's input on its way to the guest.
func (r *desktopRecorder) Write(p []byte) (int, error) {
	r.input.feed(p)
	return r.ReadWriteCloser.Write(p)
}

func (r *desktopRecorder) Close() error {
	err := r.ReadWriteCloser.Close()
	r.once.Do(func() {
		close(r.stop)
		r.wg.Wait()
		r.mu.Lock()
		rec := r.rec
		rec.EndedAt = time.Now()
		rec.InputLogIncomplete = r.input.broken
		r.mu.Unlock()
		r.done(rec)
	})
	return err
}

// rfb parse states for the viewer's half of the stream.
const (
	rfbVersion = iota
	rfbSecurity
	rfbAuth
	rfbInit
	rfbMessages
)

// rfbInput decodes the client-to-server half of an rfb session (rfc 6143)
// from chunks fed in any size, and reports the input it carries. anything
// it does not understand stops the decoding, never the relay.
type rfbInput struct {
	state   int
	buf     []byte
	broken  bool
	buttons int
	mods    map[uint32]bool
	onEvent func(desktopEvent)
}

func (f *rfbInput) feed(p []byte) {
	if f.broken {
		return
	}
	f.buf = append(f.buf, p...)
	for !f.broken {
		n := f.step()
		if n == 0 {
			return
		}
		f.buf = f.buf[n:]
	}
	f.buf = nil
}

// step consumes one handshake piece or message and returns its length, or
// 0 when more bytes are needed.
func (f *rfbInput) step() int {
	b := f.buf
	switch f.state {
	case rfbVersion:
		if len(b) < 12 {
			return 0
		}
		// 3.3 has the server pick the security type; the client sends none.
		f.state = rfbSecurity
		if string(b[:11]) == "RFB 003.003" {
			f.state = rfbInit
		}
		return 12
	case rfbSecurity:
		if len(b) < 1 {
			return 0
		}
		switch b[0] {
		case 1: // none
			f.state = rfbInit
		case 2: // vnc auth: a 16 byte response follows
			f.state = rfbAuth
		default:
			f.broken = true
		}
		return 1
	case rfbAuth:
		if len(b) < 16 {
			return 0
		}
		f.state = rfbInit
		return 16
	case rfbInit:
		if len(b) < 1 {
			return 0
		}
		f.state = rfbMessages
		return 1
	}
	if len(b) < 1 {
		return 0
	}
	need := func(n int) int {
		if len(b) < n {
			return 0
		}
		return n
	}
	switch b[0] {
	case 0: // SetPixelFormat
		return need(20)
	case 2: // SetEncodings
		if len(b) < 4 {
			return 0
		}
		return need(4 + 4*int(binary.BigEndian.Uint16(b[2:4])))
	case 3: // FramebufferUpdateRequest
		return need(10)
	case 4: // KeyEvent
		if len(b) < 8 {
			return 0
		}
		f.key(b[1] != 0, binary.BigEndian.Uint32(b[4:8]))
		return 8
	case 5: // PointerEvent
		if len(b) < 6 {
			return 0
		}
		mask := int(b[1])
		if mask != f.buttons {
			x, y := int(binary.BigEndian.Uint16(b[2:4])), int(binary.BigEndian.Uint16(b[4:6]))
			f.buttons = mask
			f.onEvent(desktopEvent{Type: "pointer", X: &x, Y: &y, Buttons: &mask})
		}
		return 6
	case 6: // ClientCutText: log the size of a paste, never its text
		if len(b) < 8 {
			return 0
		}
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if n > attachMaxPayload {
			f.broken = true
			return 0
		}
		if len(b) < 8+n {
			return 0
		}
		f.onEvent(desktopEvent{Type: "clipboard", Bytes: n})
		return 8 + n
	case 150: // EnableContinuousUpdates
		return need(10)
	case 248: // ClientFence
		if len(b) < 9 {
			return 0
		}
		return need(9 + int(b[8]))
	}
	f.broken = true
	return 0
}

// modifier keysyms.
var rfbModifiers = map[uint32]string{
	0xffe1: "shift", 0xffe2: "shift",
	0xffe3: "ctrl", 0xffe4: "ctrl",
	0xffe9: "alt", 0xffea: "alt",
	0xffe7: "meta", 0xffe8: "meta",
	0xffeb: "super", 0xffec: "super",
}

// named keysyms worth writing down.
var rfbKeyNames = map[uint32]string{
	0xff08: "BackSpace", 0xff09: "Tab", 0xff0d: "Return", 0xff1b: "Escape", 0xffff: "Delete",
	0xff50: "Home", 0xff51: "Left", 0xff52: "Up", 0xff53: "Right", 0xff54: "Down",
	0xff55: "Page_Up", 0xff56: "Page_Down", 0xff57: "End", 0xff63: "Insert",
}

func (f *rfbInput) key(down bool, sym uint32) {
	if _, ok := rfbModifiers[sym]; ok {
		if f.mods == nil {
			f.mods = make(map[uint32]bool)
		}
		f.mods[sym] = down
		return
	}
	if !down {
		return
	}
	chord := ""
	for _, m := range []string{"ctrl", "alt", "meta", "super"} {
		for sym2, name := range rfbModifiers {
			if name == m && f.mods[sym2] {
				chord += m + "+"
				break
			}
		}
	}
	if chord == "" && printable(sym) {
		f.onEvent(desktopEvent{Type: "text", Chars: 1})
		return
	}
	f.onEvent(desktopEvent{Type: "key", Key: chord + keyName(sym)})
}

func printable(sym uint32) bool {
	return (sym >= 0x20 && sym <= 0x7e) || (sym >= 0xa0 && sym <= 0xff) || sym >= 0x01000100
}

func keyName(sym uint32) string {
	if name, ok := rfbKeyNames[sym]; ok {
		return name
	}
	if sym >= 0xffbe && sym <= 0xffc9 {
		return fmt.Sprintf("F%d", sym-0xffbe+1)
	}
	if sym >= 0x20 && sym <= 0x7e {
		return string(rune(sym))
	}
	return fmt.Sprintf("0x%x", sym)
}
