package hitl

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const (
	// ActorPinned is an actor fixed on the authenticating key at mint time.
	ActorPinned ActorSource = "pinned"
	// ActorAsserted is an actor named by the caller in the Fuse-Actor
	// header, trusted because the key holds hitl:act-as (or is master).
	ActorAsserted ActorSource = "asserted"
)

const (
	// HeaderActor carries the actor id on any request.
	HeaderActor = "Fuse-Actor"
	// HeaderActorName carries the optional display name.
	HeaderActorName = "Fuse-Actor-Name"

	MaxActorIDLen   = 128
	MaxActorNameLen = 256

	// masterActorID is the actor the master token records when it names no
	// one.
	masterActorID = "master"
)

var (
	// ErrInvalidActor is a malformed Fuse-Actor or Fuse-Actor-Name header.
	ErrInvalidActor = errors.New("invalid actor")
	// ErrActorMismatch is a Fuse-Actor header that differs from the actor
	// pinned on the key.
	ErrActorMismatch = errors.New("actor does not match the key's pinned actor")
	// ErrActorNotAllowed is a Fuse-Actor header from a key without
	// hitl:act-as. it is refused rather than dropped so the caller learns
	// the label was not recorded.
	ErrActorNotAllowed = errors.New("key may not assert an actor")
	// ErrSelfApproval is a decision by the key or actor that raised the
	// intervention.
	ErrSelfApproval = errors.New("an intervention cannot be decided by the key or actor that raised it")
	// ErrActorRequired is a decision without a pinned or asserted actor on
	// an environment whose policy sets require_actor.
	ErrActorRequired = errors.New("this environment requires a named actor to decide")
)

// ParseActorHeaders reads the actor a caller named. it returns nil when
// neither header is set.
func ParseActorHeaders(id, name string) (*Actor, error) {
	if id == "" && name == "" {
		return nil, nil
	}
	a := Actor{ID: id, Name: name}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// ValidateActor checks an actor id and name, as pinned on a key or sent in
// the headers.
func ValidateActor(id, name string) error {
	return Actor{ID: id, Name: name}.validate()
}

func (a Actor) validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidActor)
	}
	if len(a.ID) > MaxActorIDLen {
		return fmt.Errorf("%w: id must be at most %d bytes", ErrInvalidActor, MaxActorIDLen)
	}
	if len(a.Name) > MaxActorNameLen {
		return fmt.Errorf("%w: name must be at most %d bytes", ErrInvalidActor, MaxActorNameLen)
	}
	if hasControl(a.ID) || hasControl(a.Name) {
		return fmt.Errorf("%w: control characters are not allowed", ErrInvalidActor)
	}
	return nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// Credential is what actor resolution needs to know about how a request
// authenticated.
type Credential struct {
	Master bool
	KeyID  string
	// Label is the key's label, recorded as the name of a key actor.
	Label string
	// Pinned is the actor fixed on the key, nil when none.
	Pinned *Actor
	// ActAs reports whether the key holds hitl:act-as.
	ActAs bool
}

// ResolveActor decides who a request is recorded as, from the credential
// and the actor the caller named (nil when none):
//
//   - a key with a pinned actor is that actor; naming anyone else is refused
//   - a key with hitl:act-as is whoever it names, recorded as asserted by it
//   - any other key is key:<id>, and naming an actor is refused
//   - the master token is whoever it names, else "master"
func ResolveActor(c Credential, named *Actor) (Actor, error) {
	switch {
	case c.Master:
		if named != nil {
			return Actor{ID: named.ID, Name: named.Name, Source: ActorAsserted}, nil
		}
		return Actor{ID: masterActorID, Source: ActorKey}, nil
	case c.Pinned != nil:
		if named != nil && named.ID != c.Pinned.ID {
			return Actor{}, ErrActorMismatch
		}
		return Actor{ID: c.Pinned.ID, Name: c.Pinned.Name, Source: ActorPinned, KeyID: c.KeyID}, nil
	case c.ActAs:
		if named == nil {
			return keyActor(c), nil
		}
		return Actor{ID: named.ID, Name: named.Name, Source: ActorAsserted, KeyID: c.KeyID}, nil
	default:
		if named != nil {
			return Actor{}, ErrActorNotAllowed
		}
		return keyActor(c), nil
	}
}

func keyActor(c Credential) Actor {
	return Actor{ID: "key:" + c.KeyID, Name: c.Label, Source: ActorKey, KeyID: c.KeyID}
}

// Named reports whether the actor is a person someone vouched for (pinned
// on a key or asserted), rather than a label derived from a credential.
func (a Actor) Named() bool {
	return a.Source == ActorPinned || a.Source == ActorAsserted
}

// isDefaultMaster is the master token naming no one. it is the operator's
// root credential, not a party to the intervention, so it is left out of the
// actor half of the self-approval check.
func (a Actor) isDefaultMaster() bool {
	return a.ID == masterActorID && a.Source == ActorKey && a.KeyID == ""
}

// CheckDecider enforces who may decide an intervention: never the key that
// raised it, never the actor that raised it, and only a named actor when
// the policy requires one.
func CheckDecider(triggeredBy, decider Actor, requireActor bool) error {
	if decider.KeyID != "" && decider.KeyID == triggeredBy.KeyID {
		return ErrSelfApproval
	}
	bothDefaultMaster := decider.isDefaultMaster() && triggeredBy.isDefaultMaster()
	if decider.ID == triggeredBy.ID && !bothDefaultMaster {
		return ErrSelfApproval
	}
	if requireActor && !decider.Named() {
		return ErrActorRequired
	}
	return nil
}
