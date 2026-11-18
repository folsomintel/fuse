package orchestrator

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// maxOrphanDestroys bounds the orphan destroys reconcile waits on at once,
// so a fleet full of orphans on a hung host holds at most this many
// requests open.
const maxOrphanDestroys = 8

// OrphanDestroyRequest is the input of the durable
// Environment/{vm_id}.DestroyOrphan handler.
type OrphanDestroyRequest struct {
	// HostID is the host that listed the orphan; "" is the single provider.
	HostID string `json:"host_id,omitempty"`
	// MaxRetries is how many failed attempts dead-letter the orphan.
	MaxRetries int `json:"max_retries"`
}

// OrphanDestroyResult is what one DestroyOrphan invocation found.
type OrphanDestroyResult struct {
	// DeadLettered is set once the orphan has failed MaxRetries times;
	// reconcile then stops sending it.
	DeadLettered bool `json:"dead_lettered,omitempty"`
}

// DurableOrphans runs reconcile's orphan destroys as the restate
// Environment/{vm_id}.DestroyOrphan handler, which keeps the retry count in
// the object's state so a restart neither resets it nor loses the dead
// letter. *durable.Client satisfies it.
type DurableOrphans interface {
	DestroyOrphan(ctx context.Context, vmID string, req OrphanDestroyRequest) (OrphanDestroyResult, error)
}

// orphanOutcomes are the results durable orphan destroys report between
// reconcile cycles; the next cycle's summary takes them.
type orphanOutcomes struct {
	destroyed, failed, deadLettered atomic.Int64
}

// SetDurableOrphans routes reconcile's orphan destroys through restate. nil
// (mode off, the default) destroys them in the reconcile pass. set it before
// Start.
func (fm *FleetManager) SetDurableOrphans(d DurableOrphans) {
	fm.mu.Lock()
	fm.durableOrphan = d
	fm.mu.Unlock()
}

func (fm *FleetManager) durableOrphans() DurableOrphans {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableOrphan
}

// sendOrphanDestroy starts the durable destroy of one orphan unless one is
// already in flight for it. it runs off the reconcile goroutine and waits for
// the invocation, so the next cycle neither doubles it up nor keeps sending
// an orphan the handler has dead-lettered.
func (fm *FleetManager) sendOrphanDestroy(d DurableOrphans, vmID, hostID string) {
	fm.mu.Lock()
	if fm.orphanInFlight[vmID] {
		fm.mu.Unlock()
		return
	}
	fm.orphanInFlight[vmID] = true
	fm.mu.Unlock()

	req := OrphanDestroyRequest{HostID: hostID, MaxRetries: fm.orphanDestroyMaxRetries}
	fm.destroySends.Add(1)
	go func() {
		defer fm.destroySends.Done()
		defer func() {
			fm.mu.Lock()
			delete(fm.orphanInFlight, vmID)
			fm.mu.Unlock()
		}()
		fm.orphanSlots <- struct{}{}
		defer func() { <-fm.orphanSlots }()

		// the handler's one host attempt is bounded by orphanDestroyTimeout;
		// the rest is room for its other steps and the round trip.
		ctx, cancel := context.WithTimeout(fm.lifetime(), 2*fm.orphanDestroyTimeout)
		defer cancel()
		res, err := d.DestroyOrphan(ctx, vmID, req)
		if err != nil {
			fm.logger.Error("durable orphan destroy failed, reconcile will resend", "vm", vmID, "err", err)
			return
		}
		if res.DeadLettered {
			// the object's state is the record; this only stops the resend.
			fm.mu.Lock()
			fm.orphanRetries[vmID] = fm.orphanDestroyMaxRetries
			fm.mu.Unlock()
		}
	}()
}

// takeOrphanOutcomes adds what durable orphan destroys reported since the
// last cycle to summary.
func (fm *FleetManager) takeOrphanOutcomes(summary *ReconcileSummary) {
	summary.OrphansDestroyed += int(fm.orphanResults.destroyed.Swap(0))
	summary.OrphansFailed += int(fm.orphanResults.failed.Swap(0))
	summary.OrphansDeadLettered += int(fm.orphanResults.deadLettered.Swap(0))
}

// OrphanCheck is the first DestroyOrphan step: whether vmID is still
// untracked. a vm the fleet tracks is never destroyed as an orphan.
func (fm *FleetManager) OrphanCheck(_ context.Context, vmID string) (bool, error) {
	fm.mu.RLock()
	_, tracked := fm.vms[vmID]
	fm.mu.RUnlock()
	return !tracked, nil
}

// OrphanDestroyOnHost is one attempt at destroying the orphan on its host,
// bounded like the in-process pass.
func (fm *FleetManager) OrphanDestroyOnHost(ctx context.Context, vmID, hostID string) error {
	provider, err := fm.providerForVM(hostID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, fm.orphanDestroyTimeout)
	defer cancel()
	return provider.Destroy(ctx, vmID)
}

// OrphanDestroyed closes out an orphan that is gone from its host: whatever
// egress backend it used is asked to release, and the next summary counts it.
func (fm *FleetManager) OrphanDestroyed(ctx context.Context, vmID string) error {
	fm.releaseOrphanEgress(ctx, vmID)
	fm.orphanResults.destroyed.Add(1)
	return nil
}

// OrphanFailed records a failed attempt. at the last one the orphan is
// dead-lettered; that write is not best effort here, since the step retries
// until it lands.
func (fm *FleetManager) OrphanFailed(ctx context.Context, vmID string, attempt int, lastErr string, deadLetter bool) error {
	fm.logger.Error("orphan destroy failed", "vm", vmID, "err", lastErr, "attempt", attempt)
	if deadLetter {
		if fm.store != nil {
			if err := fm.store.UpsertDeadLetter(ctx, DeadLetterRecord{
				Kind:        DeadLetterOrphanDestroy,
				EntityID:    vmID,
				Reason:      fmt.Sprintf("orphan destroy failed after %d attempts: %s", attempt, lastErr),
				RetryCount:  attempt,
				Payload:     dlPayload("last_error", lastErr),
				FirstSeenAt: time.Now(),
				LastSeenAt:  time.Now(),
			}); err != nil {
				return fmt.Errorf("record orphan dead letter: %w", err)
			}
		}
		if err := fm.writeEvent(ctx, "vm", vmID, "vm.orphan_dead_lettered", map[string]any{
			"retries": attempt,
			"error":   lastErr,
		}); err != nil {
			return err
		}
		fm.logger.Error("orphan dead-lettered after max retries", "vm", vmID, "retries", attempt, "err", lastErr)
		fm.orphanResults.deadLettered.Add(1)
	}
	fm.orphanResults.failed.Add(1)
	return nil
}
