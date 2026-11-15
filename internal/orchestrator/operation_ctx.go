package orchestrator

import "context"

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
