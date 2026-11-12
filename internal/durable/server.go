package durable

import (
	"context"
	"log/slog"
	"net/http"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/server"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// service names as restate sees them. an invocation addresses one as
// <name>/<key>/<handler>, e.g. Environment/fuse-abc/Ping.
const (
	// EnvironmentObject is keyed by vm id. restate runs at most one
	// exclusive handler per key at a time, which is what will serialize
	// create/destroy/fork/migrate on one environment.
	EnvironmentObject = "Environment"
	// HostObject is keyed by host id, for allocation and cordon.
	HostObject = "Host"
)

// services is every service this endpoint registers, in a fixed order.
var services = []string{EnvironmentObject, HostObject}

// Fleet is all the handlers may see of the FleetManager. kept narrow on
// purpose: handlers go through these calls, never into the manager's vm map,
// so what a durable step reads is visible in one place. *FleetManager
// satisfies it.
type Fleet interface {
	GetVM(vmID string) (orchestrator.VMInfo, bool)
	GetHost(hostID string) (orchestrator.Host, bool)
}

// PingResult is what Ping returns. it never carries more than whether the
// fleet knows the key: a Host carries its agent token, which must not leave
// through ingress.
type PingResult struct {
	Object string `json:"object"`
	Key    string `json:"key"`
	Known  bool   `json:"known"`
}

// NewEndpoint builds the restate handler endpoint with every service bound.
func NewEndpoint(cfg Config, fleet Fleet, logger *slog.Logger) *server.Restate {
	env := restate.NewObject(EnvironmentObject, retryOption(cfg, EnvironmentObject)).
		Handler("Ping", restate.NewObjectSharedHandler(
			func(ctx restate.ObjectSharedContext, _ restate.Void) (PingResult, error) {
				return ping(ctx, EnvironmentObject, func(key string) bool {
					_, ok := fleet.GetVM(key)
					return ok
				})
			}))

	host := restate.NewObject(HostObject, retryOption(cfg, HostObject)).
		Handler("Ping", restate.NewObjectSharedHandler(
			func(ctx restate.ObjectSharedContext, _ restate.Void) (PingResult, error) {
				return ping(ctx, HostObject, func(key string) bool {
					_, ok := fleet.GetHost(key)
					return ok
				})
			}))

	return server.NewRestate().
		WithLogger(logger.Handler(), true).
		Bind(env).
		Bind(host)
}

// ping looks the key up inside restate.Run, so the answer is journaled and a
// replay returns the recorded value instead of reading the fleet again. every
// later handler that reads the fleet has to follow the same rule.
func ping(ctx restate.ObjectSharedContext, object string, known func(string) bool) (PingResult, error) {
	key := restate.Key(ctx)
	ok, err := restate.Run(ctx, func(restate.RunContext) (bool, error) {
		return known(key), nil
	}, restate.WithName("lookup"))
	if err != nil {
		return PingResult{}, err
	}
	return PingResult{Object: object, Key: key, Known: ok}, nil
}

// retryOption turns the service's configured policy into its restate option.
func retryOption(cfg Config, service string) restate.ServiceDefinitionOption {
	p, ok := cfg.Retry[service]
	if !ok {
		p = DefaultRetryPolicy
	}
	return restate.WithInvocationRetryPolicy(
		restate.WithInitialRetryInterval(p.InitialInterval),
		restate.WithMaxRetryInterval(p.MaxInterval),
		restate.WithMaxRetryAttempts(uint(p.MaxAttempts)),
	)
}

// Serve runs the handler endpoint on cfg.Listen until ctx is cancelled.
func Serve(ctx context.Context, cfg Config, fleet Fleet, logger *slog.Logger) error {
	return NewEndpoint(cfg, fleet, logger).Start(ctx, cfg.Listen)
}

// Handler returns the endpoint as an http.Handler, for tests that serve it
// themselves.
func Handler(cfg Config, fleet Fleet, logger *slog.Logger) (http.Handler, error) {
	return NewEndpoint(cfg, fleet, logger).Handler()
}
