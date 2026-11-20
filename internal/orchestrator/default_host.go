package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultHostID is the id the default agent (FleetConfig.DefaultHostURL) is
// registered under. it is the id the host bootstrap (fc-agent.sh) registers
// the same local agent as, so a later explicit registration replaces this one
// and keeps its allocations instead of counting the agent twice.
const DefaultHostID = "local-fc"

// maxDefaultHostVMs is the agent's vm index range (fc-agent.py pick_index).
const maxDefaultHostVMs = 249

// defaultHostProbeTimeout bounds the capacity probe registerDefaultHost makes.
const defaultHostProbeTimeout = 10 * time.Second

// registerDefaultHost registers the default agent as host DefaultHostID when
// no host is registered yet, with the capacity the agent reports and the vm
// count the bootstrap would declare (4 per cpu), so a create on an
// orchestrator that was never given a host still goes through the
// scheduler's capacity check. a fleet that already has a host is left alone.
func (fm *FleetManager) registerDefaultHost(ctx context.Context) error {
	fm.defaultHostMu.Lock()
	defer fm.defaultHostMu.Unlock()
	fm.mu.RLock()
	registered := len(fm.hosts) > 0
	fm.mu.RUnlock()
	if registered {
		return nil
	}
	if fm.hostProviderFactory == nil {
		return errors.New("no host provider factory to rebuild the default agent's provider after a restart")
	}
	prober, ok := fm.provider.(CapacityProber)
	if !ok {
		return errors.New("the default provider cannot report its capacity")
	}
	// the agent client's own timeout is minutes; a hung agent must not hold
	// every create on a hostless fleet behind defaultHostMu that long.
	probeCtx, cancel := context.WithTimeout(ctx, defaultHostProbeTimeout)
	defer cancel()
	capacity, err := prober.Capacity(probeCtx)
	if err != nil {
		return fmt.Errorf("probe the default agent's capacity: %w", err)
	}
	if capacity.CPUs <= 0 || capacity.RamMB <= 0 || capacity.StorageGB <= 0 {
		return fmt.Errorf("the default agent reported no usable capacity (%dCPU/%dMB/%dGB)",
			capacity.CPUs, capacity.RamMB, capacity.StorageGB)
	}
	capacity.Arch = NormalizeArch(capacity.Arch)
	// gpus are served by qemu hosts only; a firecracker host never takes one.
	capacity.GPUs, capacity.GPUKind, capacity.GPUDevices = 0, "", nil
	capacity.VMCount = min(capacity.CPUs*4, maxDefaultHostVMs)
	h := Host{
		ID:       DefaultHostID,
		URL:      fm.defaultHostURL,
		Token:    fm.defaultHostToken,
		Backend:  BackendFirecracker,
		Capacity: capacity,
	}
	// in process, not through RegisterHost's restate Host object: this runs
	// inside the durable create's place step, and the Host object's Register
	// handler is this same registerHost call (HostRegister), so restate would
	// add a nested invocation and nothing else. the record's token is sealed
	// by hostToRecord with TOKEN_ENCRYPTION_KEY, which the orchestrator
	// requires. an operator registering local-fc later replaces this one.
	if err := fm.registerHost(ctx, h, fm.hostProviderFactory(h.URL, h.Token, h.Backend)); err != nil {
		return err
	}
	fm.logger.Info("no hosts were registered: registered the default agent",
		"host", h.ID, "url", h.URL, "cpus", capacity.CPUs, "ram_mb", capacity.RamMB,
		"storage_gb", capacity.StorageGB, "vm_count", capacity.VMCount)
	return nil
}
