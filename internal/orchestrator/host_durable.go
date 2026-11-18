package orchestrator

import (
	"context"
	"fmt"
)

// HostRefusals are the host op failures the api answers with a status of
// their own. the durable Host handler carries each one across restate as its
// own error code, so the api answers it as in mode off.
var HostRefusals = []error{
	ErrHostNotFound,
	ErrHostHasVMs,
}

// DurableHosts runs host registration, cordon and removal as handlers on the
// restate Host/{host_id} object, so the ops on one host run one at a time and
// each store write is a retried step. *durable.Client satisfies it.
type DurableHosts interface {
	// RegisterHost carries the host as its record, whose agent token is
	// encrypted, so the token never sits in the restate journal in the clear.
	RegisterHost(ctx context.Context, rec HostRecord) error
	SetHostState(ctx context.Context, hostID string, state HostState) error
	RemoveHost(ctx context.Context, hostID string) error
}

// SetDurableHosts routes host register, cordon, uncordon and remove through
// restate. nil (mode off, the default) runs them in process. set it before
// Start.
func (fm *FleetManager) SetDurableHosts(d DurableHosts) {
	fm.mu.Lock()
	fm.durableHost = d
	fm.mu.Unlock()
}

func (fm *FleetManager) durableHostsFor() DurableHosts {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableHost
}

// registersDurably reports whether a registration can go through restate:
// the token has to be encrypted for the journal, and the handler has to be
// able to build the host's provider from its record. without either it runs
// in process, as in mode off.
func (fm *FleetManager) registersDurably() bool {
	return len(fm.tokenEncryptionKey) == 32 && fm.hostProviderFactory != nil
}

// HostRegister is the Host/{host_id}.Register step: the host is rebuilt from
// its record with a provider of its own and registered. registering it again
// replaces it, so a retry is safe.
func (fm *FleetManager) HostRegister(ctx context.Context, rec HostRecord) error {
	h := fm.hostFromRecord(rec)
	if h.Token == "" && len(rec.TokenEncrypted) > 0 {
		return fmt.Errorf("host %s: agent token could not be decrypted", rec.ID)
	}
	return fm.registerHost(ctx, h, fm.hostProviderFactory(h.URL, h.Token, h.Backend))
}

// HostSetState is the Host/{host_id}.SetState step behind cordon and
// uncordon. setting the state it already has is a no-op write.
func (fm *FleetManager) HostSetState(ctx context.Context, hostID string, state HostState) error {
	if state != HostActive && state != HostCordoned {
		return fmt.Errorf("host %s: cannot set state %q", hostID, state)
	}
	return fm.setHostState(ctx, hostID, state)
}

// HostUnregister is the first Host/{host_id}.Remove step: the host leaves the
// registry, or the remove is refused.
func (fm *FleetManager) HostUnregister(_ context.Context, hostID string) error {
	return fm.unregisterHost(hostID)
}

// HostDeleteRecord is the second Host/{host_id}.Remove step: the host's row
// goes, retried until it does.
func (fm *FleetManager) HostDeleteRecord(ctx context.Context, hostID string) error {
	return fm.deleteHostRecord(ctx, hostID)
}
