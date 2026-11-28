package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/folsomintel/fuse/internal/egress"
)

// the refusals of SetEgressAllow, each a 4xx at the api.
var (
	// ErrEgressAllowInvalid is a list that is not ipv4 cidrs, or too long.
	ErrEgressAllowInvalid = errors.New("invalid egress allowlist")
	// ErrEgressAllowConflict is a vm whose allowlist cannot change: a proxy
	// one, whose egress is its backend, or one being migrated, whose target
	// was created with the list it had.
	ErrEgressAllowConflict = errors.New("egress allowlist cannot change")
	// ErrEgressAllowUnsupported is a provider whose host agent cannot
	// replace an allowlist.
	ErrEgressAllowUnsupported = errors.New("egress allowlist not supported by provider")
)

// egressAllowTimeout bounds the host call. the agent rewrites at most a few
// hundred iptables rules under the vm lock.
const egressAllowTimeout = 60 * time.Second

// SetEgressAllow replaces a running direct vm's outbound allowlist and
// returns the vm as it now stands. an empty list lifts the restriction.
// the host applies the list first and the fleet records the list the host
// normalized after, so a failed host call never leaves a recorded list the
// host does not enforce. a failed persist after the host applied it is an
// error too: memory already holds the new list, and the next set or a
// rebuild from the spec converges the row.
func (fm *FleetManager) SetEgressAllow(ctx context.Context, vmID string, cidrs []string) (VMInfo, error) {
	ctx, end := fm.startOperation(ctx, "set_egress_allow", vmID)
	info, err := fm.setEgressAllow(ctx, vmID, cidrs)
	end(err)
	return info, err
}

func (fm *FleetManager) setEgressAllow(ctx context.Context, vmID string, cidrs []string) (VMInfo, error) {
	allow, err := egress.NormalizeAllow(cidrs)
	if err != nil {
		return VMInfo{}, fmt.Errorf("%w: %w", ErrEgressAllowInvalid, err)
	}

	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	fm.mu.RUnlock()
	if !ok {
		return VMInfo{}, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	// one set at a time per vm, so the list recorded is the one the host
	// applied last.
	v.egressAllowMu.Lock()
	defer v.egressAllowMu.Unlock()

	fm.mu.RLock()
	state, hostID, proxy := v.state, v.hostID, v.spec.Egress.IsProxy()
	migrating := fm.migrating[vmID] > 0
	fm.mu.RUnlock()
	switch {
	case proxy:
		return VMInfo{}, fmt.Errorf("%w: vm %s uses proxy egress; an allowlist needs direct", ErrEgressAllowConflict, vmID)
	case state != VMStateRunning:
		return VMInfo{}, fmt.Errorf("%w: vm %s in state %s: changing the egress allowlist requires running", ErrVMNotRunning, vmID, state)
	case migrating:
		return VMInfo{}, fmt.Errorf("%w: vm %s is being migrated", ErrEgressAllowConflict, vmID)
	}

	p, err := fm.providerForVM(hostID)
	if err != nil {
		return VMInfo{}, err
	}
	setter, ok := p.(EgressAllowSetter)
	if !ok {
		return VMInfo{}, fmt.Errorf("%w: vm %s", ErrEgressAllowUnsupported, vmID)
	}
	callCtx, cancel := context.WithTimeout(fm.epochCtx(ctx, vmID), egressAllowTimeout)
	defer cancel()
	got, err := setter.SetEgressAllow(callCtx, vmID, allow)
	if err != nil {
		return VMInfo{}, egressAllowHostErr(vmID, err)
	}
	if len(got) == 0 {
		got = nil
	}

	fm.mu.Lock()
	if fm.vms[vmID] != v {
		fm.mu.Unlock()
		return VMInfo{}, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	// a new slice, never the old one written in place: a fork or migrate
	// that copied the spec keeps the list it copied.
	v.spec.Egress.Allow = slices.Clone(got)
	v.updatedAt = time.Now()
	fm.mu.Unlock()
	if err := fm.persistVMByID(ctx, vmID); err != nil {
		return VMInfo{}, fmt.Errorf("persist vm %s egress allowlist: %w", vmID, err)
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.egress_allow_set", map[string]any{"allow": got})

	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return v.toInfo(), nil
}

// egressAllowHostErr maps the host agent's refusals onto the fleet's
// sentinels, so none of them reaches the caller as a 500.
func egressAllowHostErr(vmID string, err error) error {
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) {
		return fmt.Errorf("set egress allowlist of vm %s: %w", vmID, err)
	}
	switch httpErr.Code {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %w", ErrEgressAllowInvalid, err)
	case http.StatusConflict:
		return fmt.Errorf("%w: vm %s: %w", ErrEgressAllowConflict, vmID, err)
	case http.StatusNotFound:
		return fmt.Errorf("%w: vm %s is not on its host: %w", ErrVMNotFound, vmID, err)
	}
	return fmt.Errorf("set egress allowlist of vm %s: %w", vmID, err)
}
