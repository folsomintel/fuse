package hitl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GrantScope is how long an approval keeps covering repeats.
type GrantScope string

const (
	// GrantOnce covers only the approved action. no grant row is written.
	GrantOnce GrantScope = "once"
	// GrantEnv covers repeats for the environment's lifetime.
	GrantEnv GrantScope = "env"
	// GrantTTL covers repeats for a fixed duration.
	GrantTTL GrantScope = "ttl"
)

// MaxGrantTTL bounds a ttl grant.
const MaxGrantTTL = 24 * time.Hour

// GrantSpec is a parsed grant: once, env or ttl:<duration>.
type GrantSpec struct {
	Scope GrantScope
	TTL   time.Duration
}

// ParseGrant reads once, env or ttl:<duration>. empty is once.
func ParseGrant(raw string) (GrantSpec, error) {
	switch raw {
	case "", string(GrantOnce):
		return GrantSpec{Scope: GrantOnce}, nil
	case string(GrantEnv):
		return GrantSpec{Scope: GrantEnv}, nil
	}
	d, ok := strings.CutPrefix(raw, "ttl:")
	if !ok {
		return GrantSpec{}, fmt.Errorf("must be once, env or ttl:<duration>, got %q", raw)
	}
	ttl, err := time.ParseDuration(d)
	if err != nil {
		return GrantSpec{}, fmt.Errorf("ttl: %q is not a duration like 30m", d)
	}
	if ttl < time.Second || ttl > MaxGrantTTL {
		return GrantSpec{}, fmt.Errorf("ttl: must be between 1s and %s, got %s", MaxGrantTTL, ttl)
	}
	return GrantSpec{Scope: GrantTTL, TTL: ttl}, nil
}

// String is the inverse of ParseGrant.
func (g GrantSpec) String() string {
	if g.Scope == GrantTTL {
		return "ttl:" + g.TTL.String()
	}
	if g.Scope == "" {
		return string(GrantOnce)
	}
	return string(g.Scope)
}

// Grant is a stored approval that covers repeats of the same action on the
// same gate.
type Grant struct {
	ID    string
	EnvID string
	Gate  string
	// Key identifies the action, see GrantKey. it never holds the argv.
	Key            string
	Scope          GrantScope
	InterventionID string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
}

// Active reports whether the grant still covers actions at now.
func (g Grant) Active(now time.Time) bool {
	return g.ExpiresAt == nil || now.Before(*g.ExpiresAt)
}

// NewGrant builds the grant an approval creates. it returns false for a
// once grant, which is never stored.
func NewGrant(in Intervention, spec GrantSpec, key string, now time.Time) (Grant, bool) {
	if spec.Scope == GrantOnce || spec.Scope == "" {
		return Grant{}, false
	}
	g := Grant{
		ID:             strings.Replace(NewID(now), "int_", "grant_", 1),
		EnvID:          in.EnvID,
		Gate:           in.Gate,
		Key:            key,
		Scope:          spec.Scope,
		InterventionID: in.ID,
		CreatedAt:      now,
	}
	if spec.Scope == GrantTTL {
		at := now.Add(spec.TTL)
		g.ExpiresAt = &at
	}
	return g, true
}

// GrantKey returns the key a grant matches on: a keyed hash of the gate and
// the action (the exact argv, or the lifecycle action), so the grants table
// never holds argv in the clear. key may be nil in dev mode.
func GrantKey(gate string, call any, key []byte) string {
	b, _ := json.Marshal(struct {
		Gate string `json:"gate"`
		Call any    `json:"call"`
	}{gate, call})
	if len(key) == 0 {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
