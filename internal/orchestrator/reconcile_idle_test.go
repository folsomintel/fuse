package orchestrator

import (
	"testing"
	"time"
)

// idleFleet builds a fleet with a single Running VM whose last activity is
// back-dated by idleFor, so idle detection can be exercised without waiting.
func idleFleet(t *testing.T, cfg FleetConfig, taskID string, idleFor time.Duration) (*FleetManager, string) {
	t.Helper()
	// A long stuck ceiling keeps the stuck detector out of the way; these
	// tests are only about the idle predicate.
	if cfg.TaskStuckTimeout == 0 {
		cfg.TaskStuckTimeout = 24 * time.Hour
	}
	fm, vmID := fleetWithRunningVM(t, cfg, taskID, idleFor)
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-idleFor)
	fm.mu.Unlock()
	return fm, vmID
}
