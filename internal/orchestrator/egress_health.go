package orchestrator

import (
	"context"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/egress"
)

// egressProbeTimeout bounds one endpoint probe. a proxy that cannot answer
// a socks5 greeting inside this is down, not slow, and the tick must not
// be held up by it.
const egressProbeTimeout = 5 * time.Second

// reconcileEgressHealth probes each proxy-mode vm's endpoint through its
// provider and records the verdict on the vm.
//
// only proxy-mode vms are probed, so a fleet that never asks for egress
// pays nothing. the probes run concurrently, like the environment
// healthcheck, because the vms share no state.
//
// unlike the environment healthcheck, a probe error is the verdict: the
// provider's Healthcheck returns an error precisely because the reason is
// what surfaces on the degraded environment. and like it, nothing acts on
// the verdict. an unhealthy proxy means the vm has no egress, which is the
// intended failure; nothing reinstalls the forward rule and nothing tears
// the vm down.
func (fm *FleetManager) reconcileEgressHealth(ctx context.Context, summary *ReconcileSummary) {
	if fm.egressRegistry == nil {
		return
	}
	type candidate struct {
		vmID   string
		status egress.Status
	}
	fm.mu.RLock()
	candidates := make([]candidate, 0, len(fm.vms))
	for id, v := range fm.vms {
		if v.state != VMStateRunning || v.egress.Mode != egress.ModeProxy {
			continue
		}
		candidates = append(candidates, candidate{vmID: id, status: v.egress})
	}
	fm.mu.RUnlock()
	if len(candidates) == 0 {
		return
	}

	type result struct {
		vmID     string
		provider string
		health   egress.Health
	}
	results := make(chan result, len(candidates))
	var wg sync.WaitGroup
	for _, c := range candidates {
		wg.Add(1)
		go func(c candidate) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, egressProbeTimeout)
			defer cancel()
			ep := egress.Endpoint{URL: c.status.Endpoint, Protocol: c.status.Protocol, Provider: c.status.Provider}
			health := egress.Health{State: egress.HealthHealthy, CheckedAt: time.Now()}
			if err := fm.egressRegistry.Healthcheck(probeCtx, ep); err != nil {
				health.State = egress.HealthUnhealthy
				health.Reason = err.Error()
			}
			results <- result{vmID: c.vmID, provider: c.status.Provider, health: health}
		}(c)
	}
	wg.Wait()
	close(results)

	counts := make(map[EgressEndpointKey]int)
	for r := range results {
		summary.EgressChecked++
		if r.health.State == egress.HealthUnhealthy {
			summary.EgressUnhealthy++
		}
		counts[EgressEndpointKey{Provider: r.provider, State: r.health.State}]++

		fm.mu.Lock()
		v, ok := fm.vms[r.vmID]
		if !ok || v.state != VMStateRunning {
			// torn down while the probe was in flight.
			fm.mu.Unlock()
			continue
		}
		previous := v.egress.Health.State
		v.egress.Health = r.health
		fm.mu.Unlock()

		if previous == r.health.State {
			continue
		}
		// provider and state only in the log: the endpoint is a host-local
		// address nobody reading logs needs, and the reason is on the wire.
		fm.logger.Info("egress health changed", "vm", r.vmID, "provider", r.provider, "from", previous, "to", r.health.State)
		if r.health.State == egress.HealthUnhealthy {
			fm.appendEventBackground("vm", r.vmID, "vm.egress_degraded", map[string]any{
				"provider": r.provider,
				"reason":   r.health.Reason,
			})
		}
	}
	summary.EgressEndpoints = counts
}
