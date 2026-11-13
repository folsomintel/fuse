package durable

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/server"
	"github.com/restatedev/sdk-go/x/tunnel"

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

	srv := server.NewRestate().
		WithLogger(logger.Handler(), true).
		Bind(env).
		Bind(host)
	// a request without a valid signature from this key gets a 401 before
	// any handler runs. self mode leaves it unset: the endpoint is loopback.
	if cfg.IdentityKey != "" {
		srv = srv.WithIdentityV1(cfg.IdentityKey)
	}
	return srv
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

// Serve runs the handler endpoint until ctx is cancelled: on cfg.Listen, or
// over the outbound tunnel in mode cloud with FUSE_RESTATE_CONNECT=tunnel.
// mode cloud also registers the deployment; in mode self the installer does.
func Serve(ctx context.Context, cfg Config, fleet Fleet, logger *slog.Logger) error {
	ep := NewEndpoint(cfg, fleet, logger)
	if cfg.Mode != ModeCloud {
		return ep.Start(ctx, cfg.Listen)
	}
	if cfg.Connect == ConnectTunnel {
		return serveTunnel(ctx, cfg, ep, logger)
	}
	go registerLoop(ctx, NewClient(cfg), cfg.DeploymentURL, logger)
	return ep.Start(ctx, cfg.Listen)
}

// tunnelDrainGrace is how long in-flight invocations get to finish over the
// tunnel on shutdown, the tunnel's own default.
const tunnelDrainGrace = 2 * time.Minute

// serveTunnel connects the endpoint to restate cloud over an outbound tunnel,
// registers the deployment url cloud hands back, and serves until ctx is
// cancelled. the tunnel verifies every request against cfg.IdentityKey.
func serveTunnel(ctx context.Context, cfg Config, ep *server.Restate, logger *slog.Logger) error {
	opts := []tunnel.Option{
		tunnel.WithRegion(cfg.Region),
		tunnel.WithEnvironment(cfg.EnvironmentID, cfg.IdentityKey),
		tunnel.WithTunnelName(cfg.TunnelName),
		tunnel.WithLogger(logger),
	}
	if cfg.TokenFile != "" {
		opts = append(opts, tunnel.WithAuthTokenFile(cfg.TokenFile))
	} else {
		opts = append(opts, tunnel.WithAuthToken(cfg.APIKey))
	}
	conn, err := tunnel.NewTunnel(ep, opts...).Connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Ready(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	go registerLoop(ctx, NewClient(cfg), conn.DeploymentURL(), logger)

	select {
	case <-ctx.Done():
		drainCtx, cancel := context.WithTimeout(context.Background(), tunnelDrainGrace)
		defer cancel()
		_ = conn.Shutdown(drainCtx)
		return nil
	case <-conn.Done():
		return conn.Err()
	}
}

// registerLoop registers uri with the admin api, retrying with backoff until
// it succeeds or ctx is cancelled. a failure is logged, not fatal: the api
// keeps serving and /ready already reports whether ingress answers.
func registerLoop(ctx context.Context, c *Client, uri string, logger *slog.Logger) {
	wait := time.Second
	for {
		err := c.Register(ctx, uri)
		if err == nil {
			logger.Info("restate deployment registered", "uri", uri, "admin", c.adminURL)
			return
		}
		logger.Warn("restate deployment registration failed, retrying", "uri", uri, "in", wait, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

// Handler returns the endpoint as an http.Handler, for tests that serve it
// themselves.
func Handler(cfg Config, fleet Fleet, logger *slog.Logger) (http.Handler, error) {
	return NewEndpoint(cfg, fleet, logger).Handler()
}
