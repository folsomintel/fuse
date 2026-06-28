package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/folsomintel/fuse/secrets"
)

// ErrHostNotFound is returned when a host operation targets an
// unregistered host ID.
var ErrHostNotFound = fmt.Errorf("host not found")

// RegisterHost adds a host to the scheduler's registry. If the host
// already exists, its capacity, URL, token, and region are updated
// (useful for heartbeat-like refreshes). The host starts in
// HostActive state.
//
// The caller must supply a Provider for this host (typically a
// firecracker.Provider constructed with the host's URL/token).
// FleetManager holds only the Provider interface to avoid import
// cycles with concrete provider packages.
func (fm *FleetManager) RegisterHost(ctx context.Context, h Host, p Provider) error {
	now := time.Now()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	h.UpdatedAt = now
	h.LastSeen = now
	if h.State == "" {
		h.State = HostActive
	}

	fm.mu.Lock()
	fm.hosts[h.ID] = &h
	if existing, ok := fm.hostProviders[h.ID]; ok {
		_ = existing.Close()
	}
	fm.hostProviders[h.ID] = p
	fm.mu.Unlock()

	if fm.store != nil {
		if err := fm.store.UpsertHost(ctx, fm.hostToRecord(h)); err != nil {
			return fmt.Errorf("persist host %s: %w", h.ID, err)
		}
	}
	fm.appendEvent(ctx, "host", h.ID, "host.registered", map[string]any{
		"url":    h.URL,
		"region": h.Region,
		"cpus":   h.Capacity.CPUs,
		"ram_mb": h.Capacity.RamMB,
	})
	return nil
}

// CordonHost marks a host as cordoned (no new VMs). Existing VMs
// are left running.
func (fm *FleetManager) CordonHost(ctx context.Context, hostID string) error {
	return fm.setHostState(ctx, hostID, HostCordoned, "host.cordoned")
}

// UncordonHost returns a cordoned or draining host to active
// scheduling.
func (fm *FleetManager) UncordonHost(ctx context.Context, hostID string) error {
	return fm.setHostState(ctx, hostID, HostActive, "host.uncordoned")
}

func (fm *FleetManager) setHostState(ctx context.Context, hostID string, state HostState, event string) error {
	fm.mu.Lock()
	h, ok := fm.hosts[hostID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrHostNotFound, hostID)
	}
	h.State = state
	h.UpdatedAt = time.Now()
	fm.mu.Unlock()

	if fm.store != nil {
		if err := fm.store.UpsertHost(ctx, fm.hostToRecord(*h)); err != nil {
			return fmt.Errorf("persist host state %s: %w", hostID, err)
		}
	}
	fm.appendEvent(ctx, "host", hostID, event, map[string]any{"state": string(state)})
	return nil
}

// RemoveHost deletes a host from the registry. It must have no VMs
// assigned to it; callers should cordon/drain and wait for VMs to
// leave before removing.
func (fm *FleetManager) RemoveHost(ctx context.Context, hostID string) error {
	fm.mu.Lock()
	_, ok := fm.hosts[hostID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrHostNotFound, hostID)
	}

	// Check that no tracked VMs reference this host.
	for _, v := range fm.vms {
		if v.hostID == hostID {
			fm.mu.Unlock()
			return fmt.Errorf("host %s still has vm %s assigned", hostID, v.id)
		}
	}

	if p, exists := fm.hostProviders[hostID]; exists {
		_ = p.Close()
		delete(fm.hostProviders, hostID)
	}
	delete(fm.hosts, hostID)
	fm.mu.Unlock()

	if fm.store != nil {
		if err := fm.store.DeleteHost(ctx, hostID); err != nil {
			return fmt.Errorf("delete host %s: %w", hostID, err)
		}
	}
	fm.appendEvent(ctx, "host", hostID, "host.removed", nil)
	return nil
}

// GetHost returns a snapshot of a registered host.
func (fm *FleetManager) GetHost(hostID string) (Host, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	h, ok := fm.hosts[hostID]
	if !ok {
		return Host{}, false
	}
	return *h, true
}

// ListHosts returns all registered hosts.
func (fm *FleetManager) ListHosts() []Host {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	out := make([]Host, 0, len(fm.hosts))
	for _, h := range fm.hosts {
		out = append(out, *h)
	}
	return out
}

// activeHosts returns a point-in-time SNAPSHOT of all hosts eligible for
// scheduling. Called under NO lock — takes its own read lock. It returns
// independent *copies* (not the live map pointers) so the pure Schedule()
// function can read host capacity/allocation without a lock while another
// goroutine mutates the real hosts under fm.mu in allocateOnHost. Returning
// the live pointers here is a data race (Schedule read vs allocateOnHost
// write); the copies make each scheduling decision operate on a consistent
// snapshot. The authoritative reservation still happens under fm.mu via
// allocateOnHost after Boot.
func (fm *FleetManager) activeHosts() []*Host {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	out := make([]*Host, 0, len(fm.hosts))
	for _, h := range fm.hosts {
		hc := *h // copy the Host (Capacity/Allocated are value structs)
		out = append(out, &hc)
	}
	return out
}

// allocateOnHost increments the allocated counters for a host after
// a successful placement and persists the new totals asynchronously
// so they survive an orchestrator restart. Must be called under
// fm.mu.Lock.
func (fm *FleetManager) allocateOnHost(hostID string, spec Spec) {
	h, ok := fm.hosts[hostID]
	if !ok {
		return
	}
	h.Allocated.CPUs += spec.CPUs
	h.Allocated.RamMB += spec.RamMB
	h.Allocated.StorageGB += spec.StorageGB
	h.Allocated.VMCount++
	h.UpdatedAt = time.Now()
	fm.persistHostRecordBackground(fm.hostToRecord(*h))
}

// deallocateOnHost decrements the allocated counters when a VM is
// destroyed and persists the new totals asynchronously. Must be
// called under fm.mu.Lock.
func (fm *FleetManager) deallocateOnHost(hostID string, spec Spec) {
	h, ok := fm.hosts[hostID]
	if !ok {
		return
	}
	h.Allocated.CPUs -= spec.CPUs
	if h.Allocated.CPUs < 0 {
		h.Allocated.CPUs = 0
	}
	h.Allocated.RamMB -= spec.RamMB
	if h.Allocated.RamMB < 0 {
		h.Allocated.RamMB = 0
	}
	h.Allocated.StorageGB -= spec.StorageGB
	if h.Allocated.StorageGB < 0 {
		h.Allocated.StorageGB = 0
	}
	h.Allocated.VMCount--
	if h.Allocated.VMCount < 0 {
		h.Allocated.VMCount = 0
	}
	h.UpdatedAt = time.Now()
	fm.persistHostRecordBackground(fm.hostToRecord(*h))
}

// providerForHost returns the cached provider for the given host ID.
// Caller must hold fm.mu (read or write).
func (fm *FleetManager) providerForHost(hostID string) (Provider, bool) {
	p, ok := fm.hostProviders[hostID]
	return p, ok
}

// listAllHostVMs collects VMs from all registered host providers.
// Used by reconcile when the fleet is in multi-host mode.
func (fm *FleetManager) listAllHostVMs(ctx context.Context) ([]Environment, error) {
	fm.mu.RLock()
	providers := make(map[string]Provider, len(fm.hostProviders))
	for id, p := range fm.hostProviders {
		providers[id] = p
	}
	fm.mu.RUnlock()

	var (
		mu   sync.Mutex
		all  []Environment
		errs []error
	)

	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			envs, err := p.List(ctx, fm.prefix)
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				all = append(all, envs...)
			}
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	if len(errs) > 0 {
		return all, fmt.Errorf("list vms from %d hosts: %d errors (first: %w)", len(providers), len(errs), errs[0])
	}
	return all, nil
}

// ── Conversion helpers ────────────────────────────────────────────

// hostToRecord builds a durable HostRecord from an in-memory Host. The
// agent bearer token is encrypted with fm.tokenEncryptionKey so it
// never reaches the database in cleartext. If the key is not
// configured the token is stored verbatim — caller is expected to
// reject this configuration when running against shared infra
// (TestFleetManager / dev only).
func (fm *FleetManager) hostToRecord(h Host) HostRecord {
	r := HostRecord{
		ID:        h.ID,
		URL:       h.URL,
		Region:    h.Region,
		State:     h.State,
		Capacity:  h.Capacity,
		Allocated: h.Allocated,
		LastSeen:  h.LastSeen,
		CreatedAt: h.CreatedAt,
		UpdatedAt: h.UpdatedAt,
	}
	if h.Token == "" {
		return r
	}
	if len(fm.tokenEncryptionKey) == 32 {
		ciphertext, err := secrets.EncryptToken(h.Token, fm.tokenEncryptionKey)
		if err == nil {
			r.TokenEncrypted = ciphertext
			return r
		}
		fm.logger.Warn("encrypt host token failed; storing plaintext", "host", h.ID, "err", err)
	}
	// Fallback: store the token bytes directly. Legacy behavior, only
	// reached when TOKEN_ENCRYPTION_KEY is unset (dev / in-memory tests).
	r.TokenEncrypted = []byte(h.Token)
	return r
}

// hostFromRecord rebuilds an in-memory Host from a HostRecord, decrypting
// the bearer token. If decryption fails (wrong key, unencrypted legacy
// row), falls back to treating the bytes as a plaintext token so
// recovered state still produces a working provider client.
func (fm *FleetManager) hostFromRecord(r HostRecord) Host {
	h := Host{
		ID:        r.ID,
		URL:       r.URL,
		Region:    r.Region,
		State:     r.State,
		Capacity:  r.Capacity,
		Allocated: r.Allocated,
		LastSeen:  r.LastSeen,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	if len(r.TokenEncrypted) == 0 {
		return h
	}
	if len(fm.tokenEncryptionKey) == 32 {
		if plain, err := secrets.DecryptToken(r.TokenEncrypted, fm.tokenEncryptionKey); err == nil {
			h.Token = plain
			return h
		}
	}
	// Fallback: ciphertext stored without encryption (legacy / dev).
	h.Token = string(r.TokenEncrypted)
	return h
}
