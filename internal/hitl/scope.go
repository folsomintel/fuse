package hitl

import (
	"fmt"
	"slices"
	"time"
)

// Scope is a permission an api key can be minted with. a key with no scopes
// (null in the store) is a full-access key, as every key was before hitl.
type Scope string

const (
	// ScopeRead lists and reads interventions and the audit log.
	ScopeRead Scope = "hitl:read"
	// ScopeDecide approves and rejects interventions.
	ScopeDecide Scope = "hitl:decide"
	// ScopeSecret answers secret requests.
	ScopeSecret Scope = "hitl:secret"
	// ScopeTakeover takes over an environment (shell access to the vm).
	ScopeTakeover Scope = "hitl:takeover"
	// ScopeActAs lets the key name the actor it acts for.
	ScopeActAs Scope = "hitl:act-as"
)

// AllScopes is every scope, in display order.
var AllScopes = []Scope{ScopeRead, ScopeDecide, ScopeSecret, ScopeTakeover, ScopeActAs}

// MaxScopedKeyTTL caps the lifetime of a key minted with scopes.
const MaxScopedKeyTTL = 24 * time.Hour

// ParseScopes checks a scope list and returns it deduplicated in display
// order.
func ParseScopes(raw []string) ([]Scope, error) {
	seen := make(map[Scope]bool, len(raw))
	for _, s := range raw {
		sc := Scope(s)
		if !slices.Contains(AllScopes, sc) {
			return nil, fmt.Errorf("unknown scope %q (want one of hitl:read, hitl:decide, hitl:secret, hitl:takeover, hitl:act-as)", s)
		}
		seen[sc] = true
	}
	out := make([]Scope, 0, len(seen))
	for _, sc := range AllScopes {
		if seen[sc] {
			out = append(out, sc)
		}
	}
	return out, nil
}

// Grants reports whether a key holding scopes may act with want. scopes nil
// is a full-access key, which holds read and takeover (it could already
// read interventions and attach) but never decide, secret or act-as: those
// have to be minted on purpose, so the agent's own key cannot approve its
// own requests. decide and secret imply read, since deciding means seeing
// what is being decided.
func Grants(scopes []Scope, want Scope) bool {
	if scopes == nil {
		return want == ScopeRead || want == ScopeTakeover
	}
	if slices.Contains(scopes, want) {
		return true
	}
	return want == ScopeRead && (slices.Contains(scopes, ScopeDecide) || slices.Contains(scopes, ScopeSecret))
}
