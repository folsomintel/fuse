package hitl

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"
)

// AuditEvent is what an audit row records.
type AuditEvent string

const (
	AuditRaised       AuditEvent = "raised"
	AuditDecided      AuditEvent = "decided"
	AuditExpired      AuditEvent = "expired"
	AuditCancelled    AuditEvent = "cancelled"
	AuditInviteMinted AuditEvent = "invite_minted"
	AuditInviteUsed   AuditEvent = "invite_used"
)

// AuditEntry is one row of the hitl audit log. it holds who did what and
// when, never content: payloads stay on the encrypted intervention row.
//
// each row carries the hash of the row before it, so editing or deleting a
// row breaks the chain at that point and VerifyChain reports where.
type AuditEntry struct {
	Seq            int64      `json:"seq"`
	Event          AuditEvent `json:"event"`
	InterventionID string     `json:"intervention_id,omitempty"`
	EnvID          string     `json:"env_id,omitempty"`
	Actor          Actor      `json:"actor"`
	RemoteAddr     string     `json:"remote_addr,omitempty"`
	At             time.Time  `json:"at"`
	PrevHash       []byte     `json:"prev_hash"`
	Hash           []byte     `json:"hash"`
}

// GenesisHash is the prev_hash of the first row.
var GenesisHash = make([]byte, sha256.Size)

// AuditTime normalizes a timestamp to what postgres stores, so a hash
// computed before the write matches one recomputed after a read.
func AuditTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// ComputeHash returns the hash of the entry's content and prev_hash. every
// field is length-prefixed so no two different rows encode the same bytes.
func (e AuditEntry) ComputeHash() []byte {
	var buf bytes.Buffer
	write := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		buf.Write(n[:])
		buf.Write(b)
	}
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], uint64(e.Seq))
	write(seq[:])
	write([]byte(e.Event))
	write([]byte(e.InterventionID))
	write([]byte(e.EnvID))
	write([]byte(e.Actor.ID))
	write([]byte(e.Actor.Name))
	write([]byte(e.Actor.Source))
	write([]byte(e.Actor.KeyID))
	write([]byte(e.RemoteAddr))
	write([]byte(AuditTime(e.At).Format(time.RFC3339Nano)))
	write(e.PrevHash)
	sum := sha256.Sum256(buf.Bytes())
	return sum[:]
}

// ChainError says where an audit chain stops verifying.
type ChainError struct {
	Seq    int64
	Reason string
}

func (e *ChainError) Error() string {
	return fmt.Sprintf("audit chain broken at seq %d: %s", e.Seq, e.Reason)
}

// VerifyChain checks a full audit log, oldest first. it returns a
// *ChainError at the first row whose content no longer matches its hash, or
// whose prev_hash does not match the row before it (an edit, a deletion or
// a reorder).
func VerifyChain(entries []AuditEntry) error {
	prev := GenesisHash
	var lastSeq int64
	for _, e := range entries {
		if e.Seq <= lastSeq {
			return &ChainError{Seq: e.Seq, Reason: "rows out of order"}
		}
		if !bytes.Equal(e.PrevHash, prev) {
			return &ChainError{Seq: e.Seq, Reason: "prev_hash does not match the previous row (a row was edited or removed before this one)"}
		}
		if !bytes.Equal(e.ComputeHash(), e.Hash) {
			return &ChainError{Seq: e.Seq, Reason: "row content does not match its hash (the row was edited)"}
		}
		prev = e.Hash
		lastSeq = e.Seq
	}
	return nil
}
