package orchestrator

import (
	"context"
	"errors"
	"sync/atomic"
)

type idempotencyKeyCtx struct{}

// WithIdempotencyKey returns ctx carrying the caller's idempotency key for a
// lifecycle op. a durable client forwards it as the restate ingress
// idempotency key, so a retried request after an orchestrator restart attaches
// to the invocation the first one started instead of starting another.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyKeyCtx{}, key)
}

// IdempotencyKey returns the key WithIdempotencyKey put on ctx, or "".
func IdempotencyKey(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyCtx{}).(string)
	return key
}

// SettledError is a lifecycle op's failure that restate recorded for its
// invocation: the handler ran and failed for good, so restate answers the
// invocation's idempotency key with this failure until the key's retention
// ends, and a retry that should run again needs a new key. a failure before
// restate answered (unreachable, a client timeout, a dropped connection, a
// context error) is not one: the invocation may still be running, and a
// retry under the same key attaches to it.
type SettledError struct{ Err error }

func (e *SettledError) Error() string { return e.Err.Error() }
func (e *SettledError) Unwrap() error { return e.Err }

// Settled wraps err as a SettledError. nil stays nil.
func Settled(err error) error {
	if err == nil {
		return nil
	}
	return &SettledError{Err: err}
}

// IsSettled reports whether err carries a SettledError.
func IsSettled(err error) bool {
	var s *SettledError
	return errors.As(err, &s)
}

type idempotencyPartCtx struct{}

type idempotencyPart struct {
	name string
	n    atomic.Int64
}

// WithIdempotencyPart returns ctx whose keyed host-agent calls are keyed as
// part name of the step: the step's key, then name, then the call's number
// under ctx. calls a step makes concurrently (its uploads) go out in no fixed
// order, so numbering them on the step's own counter would key a retried
// step's calls differently; naming each part keys them the same way.
func WithIdempotencyPart(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, idempotencyPartCtx{}, &idempotencyPart{name: name})
}

// NextIdempotencyPart returns the part name on ctx and the number of the next
// call made under it. ok is false outside WithIdempotencyPart.
func NextIdempotencyPart(ctx context.Context) (name string, n int64, ok bool) {
	p, _ := ctx.Value(idempotencyPartCtx{}).(*idempotencyPart)
	if p == nil {
		return "", 0, false
	}
	return p.name, p.n.Add(1), true
}
