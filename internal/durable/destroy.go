package durable

import (
	"context"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opDestroy is the op label on destroy's step spans and metrics.
const opDestroy = "destroy"

// Destroyer is the part of the fleet the destroy handler drives, one method
// per step. *FleetManager satisfies it.
type Destroyer interface {
	DestroyBegin(ctx context.Context, vmID, reason string) (orchestrator.DestroyTarget, error)
	DestroyReleaseEgress(ctx context.Context, vmID string) error
	DestroyOnHost(ctx context.Context, vmID, hostID string) error
	DestroyUnpublishIngress(ctx context.Context, vmID string) error
	DestroyFinish(ctx context.Context, vmID, reason string) error
	DestroyFailed(ctx context.Context, vmID, reason, lastErr string) error
}

// destroy is Environment/{vm_id}.Destroy. every step is journaled, so after a
// crash the handler resumes at the first unfinished one. the host destroy
// retries under policy; when it runs out the vm is left destroy_failed with a
// dead letter, and the invocation fails with the last error.
// the last step drops what the vm's checkpoint chain held.
func destroy(ctx restate.ObjectContext, fleet destroyFleet, policy RetryPolicy, req orchestrator.DestroyRequest) error {
	vmID := restate.Key(ctx)

	target, err := Run(ctx, opDestroy, "begin", func(c context.Context) (orchestrator.DestroyTarget, error) {
		return fleet.DestroyBegin(c, vmID, req.Reason)
	})
	if err != nil {
		return err
	}
	if target.Replaced {
		// a create cleanup whose vm id a retried create now holds.
		return nil
	}
	hostID := target.HostID
	if !target.Tracked {
		if req.HostID == "" {
			// neither tracked nor placed: an earlier destroy finished it.
			// any checkpoint chain the object still keeps for it goes too.
			return dropChain(ctx, fleet)
		}
		hostID = req.HostID
	}

	if err := step(ctx, "release_egress", func(c context.Context) error {
		return fleet.DestroyReleaseEgress(c, vmID)
	}); err != nil {
		return err
	}

	hostErr := step(ctx, "host_destroy", func(c context.Context) error {
		return fleet.DestroyOnHost(c, vmID, hostID)
	}, restate.WithInitialRetryInterval(policy.InitialInterval),
		restate.WithMaxRetryInterval(policy.MaxInterval),
		restate.WithMaxRetryAttempts(uint(policy.MaxAttempts)))
	if hostErr != nil {
		if !restate.IsTerminalError(hostErr) {
			// suspension or cancellation: let restate handle it.
			return hostErr
		}
		if err := step(ctx, "dead_letter", func(c context.Context) error {
			return fleet.DestroyFailed(c, vmID, req.Reason, hostErr.Error())
		}); err != nil {
			return err
		}
		return hostErr
	}

	if err := step(ctx, "unpublish_ingress", func(c context.Context) error {
		return fleet.DestroyUnpublishIngress(c, vmID)
	}); err != nil {
		return err
	}
	if err := step(ctx, "finish", func(c context.Context) error {
		return fleet.DestroyFinish(c, vmID, req.Reason)
	}); err != nil {
		return err
	}
	return dropChain(ctx, fleet)
}

// destroyFleet is what the destroy handler drives: its own steps, and the
// checkpoint chain it drops at the end.
type destroyFleet interface {
	Destroyer
	Checkpoints
}

// step is Run for a step with no result.
func step(ctx restate.Context, name string, fn func(context.Context) error, opts ...restate.RunOption) error {
	_, err := Run(ctx, opDestroy, name, func(c context.Context) (restate.Void, error) {
		return restate.Void{}, fn(c)
	}, opts...)
	return err
}

// Destroy runs Environment/{vmID}.Destroy and waits for it. the invocation
// keeps running if ctx ends first.
func (c *Client) Destroy(ctx context.Context, vmID string, req orchestrator.DestroyRequest) error {
	_, err := ingress.Object[orchestrator.DestroyRequest, restate.Void](c.ingress, EnvironmentObject, vmID, "Destroy").
		Request(ctx, req, callerKey(ctx)...)
	return err
}

// callerKey forwards the api caller's idempotency key on ctx as the ingress
// idempotency key, so a retried request attaches to the invocation the first
// one started. restate scopes the key to the handler and object key.
func callerKey(ctx context.Context) []ingress.RequestOption {
	key := orchestrator.IdempotencyKey(ctx)
	if key == "" {
		return nil
	}
	return []ingress.RequestOption{restate.WithIdempotencyKey(key)}
}

// SendDestroy enqueues Environment/{vmID}.Destroy without waiting.
func (c *Client) SendDestroy(ctx context.Context, vmID string, req orchestrator.DestroyRequest) error {
	_, err := ingress.ObjectSend[orchestrator.DestroyRequest](c.ingress, EnvironmentObject, vmID, "Destroy").
		Send(ctx, req)
	return err
}
