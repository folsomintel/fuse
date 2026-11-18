package durable

import (
	"context"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opOrphan is the op label on orphan destroy's step spans and metrics.
const opOrphan = "orphan"

// orphanKey is the Environment object's orphan destroy state.
const orphanKey = "orphan"

// Orphans is the part of the fleet the orphan destroy handler drives, one
// method per step. *FleetManager satisfies it.
type Orphans interface {
	OrphanCheck(ctx context.Context, vmID string) (bool, error)
	OrphanDestroyOnHost(ctx context.Context, vmID, hostID string) error
	OrphanDestroyed(ctx context.Context, vmID string) error
	OrphanFailed(ctx context.Context, vmID string, attempt int, lastErr string, deadLetter bool) error
}

// orphanState is what the Environment object keeps between orphan destroy
// attempts. it replaces the in-memory retry count of mode off, so a restart
// mid-retry neither resets the count nor loses the dead letter.
type orphanState struct {
	Retries int `json:"retries"`
}

// destroyOrphan is Environment/{vm_id}.DestroyOrphan, sent by reconcile for
// a vm a host lists and the fleet does not track. it makes one attempt per
// invocation: reconcile's next cycle is the retry, as in mode off, and the
// failed attempts are counted in object state until the orphan is
// dead-lettered. a dead-lettered orphan is never attempted again.
func destroyOrphan(ctx restate.ObjectContext, fleet Orphans, req orchestrator.OrphanDestroyRequest) (orchestrator.OrphanDestroyResult, error) {
	vmID := restate.Key(ctx)
	st, getErr := restate.Get[*orphanState](ctx, orphanKey)
	if getErr != nil {
		return orchestrator.OrphanDestroyResult{}, getErr
	}
	if st == nil {
		st = &orphanState{}
	}
	if st.Retries >= req.MaxRetries {
		return orchestrator.OrphanDestroyResult{DeadLettered: true}, nil
	}

	orphan, err := Run(ctx, opOrphan, "check", func(c context.Context) (bool, error) {
		return fleet.OrphanCheck(c, vmID)
	})
	if err != nil {
		return orchestrator.OrphanDestroyResult{}, err
	}
	if !orphan {
		restate.Clear(ctx, orphanKey)
		return orchestrator.OrphanDestroyResult{}, nil
	}

	// the attempt's outcome is the step's value, not its error, so a failure
	// is journaled once and counted instead of retried inside the step.
	lastErr, err := Run(ctx, opOrphan, "host_destroy", func(c context.Context) (string, error) {
		if err := fleet.OrphanDestroyOnHost(c, vmID, req.HostID); err != nil {
			return err.Error(), nil
		}
		return "", nil
	})
	if err != nil {
		return orchestrator.OrphanDestroyResult{}, err
	}
	if lastErr == "" {
		restate.Clear(ctx, orphanKey)
		err := runStep(ctx, opOrphan, "finish", func(c context.Context) error {
			return fleet.OrphanDestroyed(c, vmID)
		})
		return orchestrator.OrphanDestroyResult{}, err
	}

	st.Retries++
	restate.Set(ctx, orphanKey, st)
	dead := st.Retries >= req.MaxRetries
	if err := runStep(ctx, opOrphan, "failed", func(c context.Context) error {
		return fleet.OrphanFailed(c, vmID, st.Retries, lastErr, dead)
	}); err != nil {
		return orchestrator.OrphanDestroyResult{}, err
	}
	return orchestrator.OrphanDestroyResult{DeadLettered: dead}, nil
}

// DestroyOrphan runs Environment/{vmID}.DestroyOrphan and waits for it. the
// host attempt can take the whole orphan destroy timeout, so it goes through
// the client with no request timeout of its own; ctx bounds the wait.
func (c *Client) DestroyOrphan(ctx context.Context, vmID string, req orchestrator.OrphanDestroyRequest) (orchestrator.OrphanDestroyResult, error) {
	return ingress.Object[orchestrator.OrphanDestroyRequest, orchestrator.OrphanDestroyResult](c.longIngress, EnvironmentObject, vmID, "DestroyOrphan").
		Request(ctx, req)
}

var _ orchestrator.DurableOrphans = (*Client)(nil)
