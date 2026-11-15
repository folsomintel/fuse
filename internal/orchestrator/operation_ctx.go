package orchestrator

import (
	"context"
	"sync/atomic"
)

type idempotencyKeyCtx struct{}

type stepReporterCtx struct{}

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

// WithStepReporter returns ctx whose lifecycle op reports each step it starts
// to fn. the api shows the latest one as an operation's current step.
func WithStepReporter(ctx context.Context, fn func(step string)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, stepReporterCtx{}, fn)
}

// reportStep tells ctx's step reporter, if any, that step has started.
func reportStep(ctx context.Context, step string) {
	if fn, ok := ctx.Value(stepReporterCtx{}).(func(string)); ok {
		fn(step)
	}
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
