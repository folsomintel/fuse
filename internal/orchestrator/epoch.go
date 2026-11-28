package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// epoch fencing. a vm id is reused when an environment is destroyed and
// created again under the same task id, so a host-agent request made for one
// incarnation can reach the next: one queued behind the agent's vm lock
// after its caller gave up, or one made through a handle that outlived its
// vm. every incarnation has an epoch, and a mutating host request carries the
// one it is for (hostwire.EpochHeader). the agent refuses a request whose
// epoch is not the vm's with a 412, and a create for an epoch no newer than
// the newest it has made under the id. see agentlib.epoch_fence.
//
// the epoch is the incarnation's createdAt in unix milliseconds. it needs no
// column of its own: createdAt is fixed when the vm is first tracked,
// persisted before anything reaches a host, and kept across restarts, and a
// later incarnation of the same id is created later. milliseconds, because
// the store keeps microseconds and any precision it dropped would change the
// epoch across a restart. an orchestrator clock stepped back past an
// earlier incarnation's createdAt makes the new create a 412, which fails
// closed.

// VMEpoch is the epoch of the incarnation created at createdAt, or 0 when
// createdAt is unset.
func VMEpoch(createdAt time.Time) int64 {
	if createdAt.IsZero() {
		return 0
	}
	return createdAt.UnixMilli()
}

func (v *vm) epoch() int64 { return VMEpoch(v.createdAt) }

type vmEpochCtxKey struct{}

// WithVMEpoch returns ctx whose host-agent requests are for incarnation
// epoch. an epoch of 0 leaves ctx unchanged.
func WithVMEpoch(ctx context.Context, epoch int64) context.Context {
	if epoch <= 0 {
		return ctx
	}
	return context.WithValue(ctx, vmEpochCtxKey{}, epoch)
}

// VMEpochFrom is the epoch ctx carries, or 0.
func VMEpochFrom(ctx context.Context) int64 {
	e, _ := ctx.Value(vmEpochCtxKey{}).(int64)
	return e
}

// epochCtx is ctx for host requests about the tracked vm vmID. an untracked
// vm leaves ctx unchanged, so its requests are not fenced.
func (fm *FleetManager) epochCtx(ctx context.Context, vmID string) context.Context {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	var epoch int64
	if ok {
		epoch = v.epoch()
	}
	fm.mu.RUnlock()
	return WithVMEpoch(ctx, epoch)
}

// Epoched is an environment handle that knows its incarnation's epoch: the
// one it was created or looked up under, else the one its host reported.
type Epoched interface {
	Epoch() int64
}

// EnvEpoch is env's epoch, or 0 when it has none or does not know it.
func EnvEpoch(env Environment) int64 {
	if e, ok := env.(Epoched); ok {
		return e.Epoch()
	}
	return 0
}

// IsStaleEpoch reports whether err is a host agent refusing a request for an
// incarnation other than the vm it has under the id.
func IsStaleEpoch(err error) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) && statusErr.Code == http.StatusPreconditionFailed
}

// ErrOtherIncarnation is a host holding a vm under the id that belongs to an
// incarnation other than the one asked for.
var ErrOtherIncarnation = errors.New("host holds another incarnation of the vm")

// IncarnationError is ErrOtherIncarnation for one vm: Host is the epoch of
// the vm the host has, Want the one asked for.
type IncarnationError struct {
	VMID       string
	Host, Want int64
}

func (e *IncarnationError) Error() string {
	return fmt.Sprintf("%s: vm %s is epoch %d on its host, not %d", ErrOtherIncarnation, e.VMID, e.Host, e.Want)
}

func (e *IncarnationError) Unwrap() error { return ErrOtherIncarnation }

// CheckIncarnation is the error a lookup under ctx returns for a vm its host
// reports at epoch host: nil when either epoch is unknown or they match.
func CheckIncarnation(ctx context.Context, vmID string, host int64) error {
	want := VMEpochFrom(ctx)
	if want == 0 || host == 0 || want == host {
		return nil
	}
	return &IncarnationError{VMID: vmID, Host: host, Want: want}
}
