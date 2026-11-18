package durable

import (
	"context"
	"errors"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opHost is the op label on the host ops' step spans and metrics.
const opHost = "host"

// hostErrorBase+i is the error code orchestrator.HostRefusals[i] crosses
// restate as. clear of the codes the other handlers number from.
const hostErrorBase restate.Code = 570

// Hosts is the part of the fleet the Host handlers drive, one method per
// step. *FleetManager satisfies it.
type Hosts interface {
	HostRegister(ctx context.Context, rec orchestrator.HostRecord) error
	HostSetState(ctx context.Context, hostID string, state orchestrator.HostState) error
	HostUnregister(ctx context.Context, hostID string) error
	HostDeleteRecord(ctx context.Context, hostID string) error
}

// hostRefusal marks a host refusal terminal under its own code, so a step
// does not retry what a retry cannot change.
func hostRefusal(err error) error {
	for i, s := range orchestrator.HostRefusals {
		if errors.Is(err, s) {
			return restate.ToTerminalError(err, restate.WithErrorCode(hostErrorBase+restate.Code(i)))
		}
	}
	return err
}

// hostError turns a Host handler's failure back into the fleet sentinel its
// code names.
func hostError(err error) error {
	var ierr ingress.Error
	if !errors.As(err, &ierr) {
		return err
	}
	if i := int(ierr.Code()) - int(hostErrorBase); i >= 0 && i < len(orchestrator.HostRefusals) {
		return &sentinelError{msg: ierr.Message(), sentinel: orchestrator.HostRefusals[i]}
	}
	return err
}

// hostRegister is Host/{host_id}.Register. the registry and the row are
// written in one step, which re-registers on retry.
func hostRegister(ctx restate.ObjectContext, fleet Hosts, rec orchestrator.HostRecord) error {
	return runStep(ctx, opHost, "register", func(c context.Context) error {
		return fleet.HostRegister(c, rec)
	})
}

// hostSetState is Host/{host_id}.SetState, behind cordon and uncordon.
func hostSetState(ctx restate.ObjectContext, fleet Hosts, state orchestrator.HostState) error {
	hostID := restate.Key(ctx)
	return runStep(ctx, opHost, "set_state", func(c context.Context) error {
		return hostRefusal(fleet.HostSetState(c, hostID, state))
	})
}

// hostRemove is Host/{host_id}.Remove. the host leaves the registry in one
// step and its row goes in the next, so a store failure retries the delete
// rather than finding the host already gone and refusing.
func hostRemove(ctx restate.ObjectContext, fleet Hosts) error {
	hostID := restate.Key(ctx)
	if err := runStep(ctx, opHost, "unregister", func(c context.Context) error {
		return hostRefusal(fleet.HostUnregister(c, hostID))
	}); err != nil {
		return err
	}
	return runStep(ctx, opHost, "delete_record", func(c context.Context) error {
		return fleet.HostDeleteRecord(c, hostID)
	})
}

// RegisterHost runs Host/{rec.ID}.Register and waits for it.
func (c *Client) RegisterHost(ctx context.Context, rec orchestrator.HostRecord) error {
	_, err := ingress.Object[orchestrator.HostRecord, restate.Void](c.ingress, HostObject, rec.ID, "Register").
		Request(ctx, rec, callerKey(ctx)...)
	return hostError(err)
}

// SetHostState runs Host/{hostID}.SetState and waits for it.
func (c *Client) SetHostState(ctx context.Context, hostID string, state orchestrator.HostState) error {
	_, err := ingress.Object[orchestrator.HostState, restate.Void](c.ingress, HostObject, hostID, "SetState").
		Request(ctx, state, callerKey(ctx)...)
	return hostError(err)
}

// RemoveHost runs Host/{hostID}.Remove and waits for it.
func (c *Client) RemoveHost(ctx context.Context, hostID string) error {
	_, err := ingress.Object[restate.Void, restate.Void](c.ingress, HostObject, hostID, "Remove").
		Request(ctx, restate.Void{}, callerKey(ctx)...)
	return hostError(err)
}

var _ orchestrator.DurableHosts = (*Client)(nil)
