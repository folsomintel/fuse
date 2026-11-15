package durable

import (
	"context"
	"errors"
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// op labels on the drain and expiry step spans and metrics.
const (
	opDrain  = "drain"
	opExpire = "expire"
)

// drainFailedCode is the error code a drain command that failed in the guest
// crosses restate as, so the client hands the api the fleet's message
// unchanged. 480-486 are the snapshot sentinels, 487 a token rotation's
// refusal.
const drainFailedCode restate.Code = 488

// expiryKey is the Environment object's expiry state.
const expiryKey = "expiry"

// Expiry is the part of the fleet the drain and expiry handlers drive, one
// method per step. *FleetManager satisfies it.
type Expiry interface {
	DrainCheck(ctx context.Context, vmID string) error
	DrainBegin(ctx context.Context, vmID string) (orchestrator.DrainPlan, error)
	DrainCommand(ctx context.Context, vmID string) string
	ExpiryCheck(ctx context.Context, vmID, strike string) (orchestrator.ExpiryVerdict, error)
}

// ExpireRequest is the input of Environment/{vm_id}.Expire.
type ExpireRequest struct {
	// Token names the timer this invocation is. only the timer the object
	// last scheduled runs a check; any other is stale and does nothing. an
	// empty token is an arm: it checks now and takes over from whatever
	// timer is pending.
	Token string `json:"token,omitempty"`
}

// expiryState is what the Environment object keeps between expiry checks.
// it replaces the strike maps reconcile holds in memory in mode off, so a
// restart neither resets a strike nor loses a timer.
type expiryState struct {
	// Token is the pending timer's.
	Token string `json:"token"`
	// Strike is the reason the last check found the vm past its window
	// for, or empty. the next check past the same window reaps.
	Strike string `json:"strike,omitempty"`
}

// drain is Environment/{vm_id}.Drain: the vm goes draining, the grace timer
// is armed, then the drain command runs. the timer is armed before the
// command, so a command that fails, or an orchestrator that dies under it,
// still ends in a destroy once the grace is up. the timer is a delayed send,
// not a sleep: a DELETE during the grace runs at once instead of queueing
// behind this invocation on the object.
//
// the command runs once: a failure is the caller's result, not retried. a
// crash during it replays the step, so the command can run twice.
func drain(ctx restate.ObjectContext, fleet Expiry, policy RetryPolicy) error {
	vmID := restate.Key(ctx)
	if err := runStep(ctx, opDrain, "check", func(c context.Context) error {
		return permanent(fleet.DrainCheck(c, vmID))
	}); err != nil {
		return err
	}
	plan, err := Run(ctx, opDrain, "begin", func(c context.Context) (orchestrator.DrainPlan, error) {
		plan, err := fleet.DrainBegin(c, vmID)
		return plan, permanent(err)
	}, retryUnder(policy)...)
	if err != nil {
		return err
	}
	st, err := loadExpiry(ctx)
	if err != nil {
		return err
	}
	scheduleExpiry(ctx, vmID, st, plan.Grace)
	if !plan.Command {
		return nil
	}
	msg, err := Run(ctx, opDrain, "command", func(c context.Context) (string, error) {
		return fleet.DrainCommand(c, vmID), nil
	})
	if err != nil {
		return err
	}
	if msg != "" {
		return restate.ToTerminalError(fmt.Errorf("drain vm %s: %s", vmID, msg), restate.WithErrorCode(drainFailedCode))
	}
	return nil
}

// envExpire is Environment/{vm_id}.Expire, the ttl, idle and drain-grace timer
// of one environment. each check runs as a journaled step and schedules the
// next one as a delayed send to itself, never one long sleep, so a days-long
// lifetime pins no invocation to an old deployment. a vm the check reaps is
// destroyed by a send to Destroy on the same object; one with nothing left to
// watch has its state cleared.
func envExpire(ctx restate.ObjectContext, fleet Expiry, policy RetryPolicy, req ExpireRequest) error {
	vmID := restate.Key(ctx)
	st, err := loadExpiry(ctx)
	if err != nil {
		return err
	}
	if req.Token != "" && req.Token != st.Token {
		// a timer an arm or a later check replaced.
		return nil
	}
	v, err := Run(ctx, opExpire, "check", func(c context.Context) (orchestrator.ExpiryVerdict, error) {
		return fleet.ExpiryCheck(c, vmID, st.Strike)
	}, unbounded(policy)...)
	if err != nil {
		return err
	}
	switch {
	case v.Reap:
		restate.Clear(ctx, expiryKey)
		restate.ObjectSend(ctx, EnvironmentObject, vmID, "Destroy").
			Send(orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonExpired})
	case v.In > 0:
		st.Strike = v.Strike
		scheduleExpiry(ctx, vmID, st, v.In)
	default:
		restate.Clear(ctx, expiryKey)
	}
	return nil
}

func loadExpiry(ctx restate.ObjectContext) (expiryState, error) {
	st, err := restate.Get[*expiryState](ctx, expiryKey)
	if err != nil || st == nil {
		return expiryState{}, err
	}
	return *st, nil
}

// scheduleExpiry sends the next Expire check after delay under a fresh token,
// which makes any timer already pending a no-op.
func scheduleExpiry(ctx restate.ObjectContext, vmID string, st expiryState, delay time.Duration) {
	st.Token = restate.UUID(ctx).String()
	restate.Set(ctx, expiryKey, st)
	restate.ObjectSend(ctx, EnvironmentObject, vmID, "Expire").
		Send(ExpireRequest{Token: st.Token}, restate.WithDelay(delay))
}

// Drain runs Environment/{vmID}.Drain and waits for it. it may queue behind
// a long snapshot on the same vm, so it goes through the client with no
// request timeout of its own; ctx bounds the wait.
func (c *Client) Drain(ctx context.Context, vmID string) error {
	_, err := ingress.Object[restate.Void, restate.Void](c.longIngress, EnvironmentObject, vmID, "Drain").
		Request(ctx, restate.Void{}, callerKey(ctx)...)
	var ierr ingress.Error
	if errors.As(err, &ierr) && ierr.Code() == drainFailedCode {
		return errors.New(ierr.Message())
	}
	return fleetError(err)
}

// SendArmExpiry enqueues Environment/{vmID}.Expire with no token: a check
// now that takes over from any pending timer.
func (c *Client) SendArmExpiry(ctx context.Context, vmID string) error {
	_, err := ingress.ObjectSend[ExpireRequest](c.ingress, EnvironmentObject, vmID, "Expire").
		Send(ctx, ExpireRequest{})
	return err
}

var _ orchestrator.DurableExpiry = (*Client)(nil)
