package orchestrator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// ErrRecordingNotFound is returned for an unknown recording id.
var ErrRecordingNotFound = errors.New("recording not found")

// fuse-attach/1 frame types the recorder reads. see docs/attach.md.
const (
	attachFrameStdout byte = 1
	attachFrameStderr byte = 2
	attachFrameResize byte = 3
	attachFrameHeader      = 8
	attachMaxPayload       = 1 << 20
)

// maxCastBytes caps one recorded session. past it the cast stops growing
// and is marked truncated; the shell itself keeps working.
const maxCastBytes = 8 << 20

// cast is a finished asciinema v2 recording.
type cast struct {
	data      []byte
	startedAt time.Time
	endedAt   time.Time
	truncated bool
}

// castWriter builds an asciinema v2 cast: a json header line, then one
// [elapsed, code, data] line per event. only output and resizes are kept,
// never keystrokes, so what a human types (a password at a prompt that does
// not echo) is not in it.
type castWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	start     time.Time
	truncated bool
}

func newCastWriter(title string, cols, rows uint16, now time.Time) *castWriter {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	w := &castWriter{start: now}
	header, _ := json.Marshal(map[string]any{
		"version":   2,
		"width":     cols,
		"height":    rows,
		"timestamp": now.Unix(),
		"title":     title,
	})
	w.buf.Write(header)
	w.buf.WriteByte('\n')
	return w
}

func (w *castWriter) event(code, data string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return
	}
	line, _ := json.Marshal([]any{time.Since(w.start).Seconds(), code, data})
	if w.buf.Len()+len(line)+1 > maxCastBytes {
		w.truncated = true
		return
	}
	w.buf.Write(line)
	w.buf.WriteByte('\n')
}

func (w *castWriter) finish(now time.Time) cast {
	w.mu.Lock()
	defer w.mu.Unlock()
	return cast{data: bytes.Clone(w.buf.Bytes()), startedAt: w.start, endedAt: now, truncated: w.truncated}
}

// frameReader decodes fuse-attach/1 frames from a byte stream fed in
// arbitrary chunks, handing each whole frame to onFrame. a malformed stream
// stops the decoding, never the relay.
type frameReader struct {
	buf     []byte
	broken  bool
	onFrame func(typ byte, payload []byte)
}

func (f *frameReader) feed(p []byte) {
	if f.broken {
		return
	}
	f.buf = append(f.buf, p...)
	for len(f.buf) >= attachFrameHeader {
		n := binary.BigEndian.Uint32(f.buf[4:8])
		if n > attachMaxPayload {
			f.broken, f.buf = true, nil
			return
		}
		end := attachFrameHeader + int(n)
		if len(f.buf) < end {
			return
		}
		f.onFrame(f.buf[0], f.buf[attachFrameHeader:end])
		f.buf = f.buf[end:]
	}
}

func encodeAttachFrame(typ byte, payload []byte) []byte {
	out := make([]byte, attachFrameHeader+len(payload))
	out[0] = typ
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[attachFrameHeader:], payload)
	return out
}

// recordedStream is an attach stream with a recorder on it. reads (guest to
// human) are recorded and start with the banner; writes (human to guest)
// are only read for resizes.
type recordedStream struct {
	io.ReadWriteCloser
	banner []byte
	rec    *castWriter
	out    *frameReader
	in     *frameReader
	once   sync.Once
	done   func(cast)
}

func newRecordedStream(s io.ReadWriteCloser, in hitl.Intervention, by hitl.Actor, spec AttachSpec, done func(cast)) *recordedStream {
	rec := newCastWriter("fuse takeover "+in.ID, spec.Cols, spec.Rows, time.Now())
	text := fmt.Sprintf("\r\n[fuse] this session is recorded (takeover %s, environment %s, as %s).\r\n"+
		"[fuse] ask for credentials through a secret request, never type them here.\r\n\r\n", in.ID, in.EnvID, by.ID)
	rec.event("o", text)
	r := &recordedStream{
		ReadWriteCloser: s,
		banner:          encodeAttachFrame(attachFrameStdout, []byte(text)),
		rec:             rec,
		done:            done,
	}
	r.out = &frameReader{onFrame: func(typ byte, p []byte) {
		if typ == attachFrameStdout || typ == attachFrameStderr {
			rec.event("o", string(p))
		}
	}}
	r.in = &frameReader{onFrame: func(typ byte, p []byte) {
		var size struct {
			Rows uint16 `json:"rows"`
			Cols uint16 `json:"cols"`
		}
		if typ == attachFrameResize && json.Unmarshal(p, &size) == nil && size.Rows > 0 && size.Cols > 0 {
			rec.event("r", fmt.Sprintf("%dx%d", size.Cols, size.Rows))
		}
	}}
	return r
}

func (r *recordedStream) Read(p []byte) (int, error) {
	if len(r.banner) > 0 {
		n := copy(p, r.banner)
		r.banner = r.banner[n:]
		return n, nil
	}
	n, err := r.ReadWriteCloser.Read(p)
	if n > 0 {
		r.out.feed(p[:n])
	}
	return n, err
}

func (r *recordedStream) Write(p []byte) (int, error) {
	r.in.feed(p)
	return r.ReadWriteCloser.Write(p)
}

func (r *recordedStream) Close() error {
	err := r.ReadWriteCloser.Close()
	r.once.Do(func() { r.done(r.rec.finish(time.Now())) })
	return err
}

// saveRecording seals a finished session and links it from the audit log.
// a store failure is logged; the session already happened and failing
// loudly here would not undo it.
func (fm *FleetManager) saveRecording(ctx context.Context, in hitl.Intervention, by hitl.Actor, remoteAddr string, c cast) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	id := fmt.Sprintf("rec_%016x%s", c.startedAt.UnixNano(), hex.EncodeToString(b[:]))
	actor, _ := json.Marshal(by)
	data, sealed, err := fm.seal(c.data)
	if err != nil {
		fm.logger.Error("seal takeover recording failed", "intervention", in.ID, "err", err)
		return
	}
	if !sealed {
		fm.logger.Warn("takeover recording stored unencrypted; set TOKEN_ENCRYPTION_KEY", "intervention", in.ID)
	}
	rec := RecordingRecord{
		ID:             id,
		InterventionID: in.ID,
		EnvID:          in.EnvID,
		Actor:          actor,
		StartedAt:      c.startedAt,
		EndedAt:        c.endedAt,
		Size:           int64(len(c.data)),
		Truncated:      c.truncated,
		Sealed:         sealed,
		Data:           data,
	}
	if err := fm.store.SaveHitlRecording(ctx, rec); err != nil {
		fm.logger.Error("save takeover recording failed", "intervention", in.ID, "err", err)
		return
	}
	fm.recordAudit(ctx, hitl.AuditEntry{
		Event: hitl.AuditTakeoverSession, InterventionID: in.ID, EnvID: in.EnvID, Actor: by, RemoteAddr: remoteAddr, RecordingID: id,
	}, fm.webhookTargetsFor(in.EnvID), &in)
}

// Recording is a decrypted takeover cast and what it belongs to.
type Recording struct {
	ID             string
	InterventionID string
	EnvID          string
	Actor          hitl.Actor
	StartedAt      time.Time
	EndedAt        time.Time
	Truncated      bool
	Cast           []byte
}

// GetRecording returns one takeover session, decrypted with the
// orchestrator key.
func (fm *FleetManager) GetRecording(ctx context.Context, id string) (Recording, error) {
	rec, err := fm.store.GetHitlRecording(ctx, id)
	if err != nil {
		return Recording{}, err
	}
	data, err := fm.unseal(rec.Data, rec.Sealed)
	if err != nil {
		return Recording{}, fmt.Errorf("recording %s: %w", id, err)
	}
	out := Recording{
		ID: rec.ID, InterventionID: rec.InterventionID, EnvID: rec.EnvID,
		StartedAt: rec.StartedAt, EndedAt: rec.EndedAt, Truncated: rec.Truncated, Cast: data,
	}
	if len(rec.Actor) > 0 {
		_ = json.Unmarshal(rec.Actor, &out.Actor)
	}
	return out, nil
}
