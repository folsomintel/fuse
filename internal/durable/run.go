package durable

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	restate "github.com/restatedev/sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/folsomintel/fuse/internal/telemetry"
)

// StepMetrics records durable steps. *metrics.PrometheusMetrics satisfies it
// as orchestrator_step_duration_seconds and orchestrator_step_retries_total.
type StepMetrics interface {
	// StepCompleted is called once per execution of a step body, successful
	// or not. a step replayed from the journal does not execute and is not
	// counted.
	StepCompleted(op, step string, d time.Duration, err error)
	// StepRetried is called when a step body fails with a retryable error,
	// i.e. restate will run it again.
	StepRetried(op, step string)
}

var stepMetrics atomic.Pointer[StepMetrics]

// SetStepMetrics installs the recorder Run reports to. nil turns it off.
func SetStepMetrics(m StepMetrics) {
	if m == nil {
		stepMetrics.Store(nil)
		return
	}
	stepMetrics.Store(&m)
}

// Run executes fn as the journaled step `step` of operation `op`. every
// hostwire call a ported operation makes goes through it.
//
// fn gets a context carrying the api request id (from the invocation's
// X-Request-ID header) and a span for the step, parented on restate's
// invocation attempt span, so host-agent calls made inside it join the api
// request's trace. a step replayed from the journal is not run again, so it
// opens no span and records no metric.
func Run[T any](ctx restate.Context, op, step string, fn func(context.Context) (T, error), opts ...restate.RunOption) (T, error) {
	opts = append([]restate.RunOption{restate.WithName(step)}, opts...)
	out, err := restate.Run(ctx, func(rc restate.RunContext) (T, error) {
		start := time.Now()
		sctx := telemetry.WithRequestID(rc, invocationRequestID(rc.Request()))
		sctx, span := telemetry.Tracer().Start(sctx, op+"."+step,
			trace.WithAttributes(
				attribute.String("fuse.op", op),
				attribute.String("fuse.step", step),
				attribute.String("restate.invocation.id", rc.Request().ID),
			))
		v, err := fn(sctx)
		telemetry.EndSpan(span, err)
		if p := stepMetrics.Load(); p != nil {
			(*p).StepCompleted(op, step, time.Since(start), err)
			if err != nil && !restate.IsTerminalError(err) {
				(*p).StepRetried(op, step)
			}
		}
		return v, err
	}, opts...)
	// a nil TerminalError converts to a nil error.
	return out, err
}

// invocationRequestID returns the X-Request-ID the invocation was started
// with. restate keeps the ingress request's headers on the invocation; header
// names may arrive lowercased, so the match ignores case. any ingress client
// can set it, so a value outside telemetry.RequestIDPattern is dropped.
func invocationRequestID(req *restate.Request) string {
	if req == nil || req.Headers == nil {
		return ""
	}
	for k, v := range req.Headers.Iter() {
		if strings.EqualFold(k, telemetry.RequestIDHeader) && telemetry.ValidRequestID(v) {
			return v
		}
	}
	return ""
}
