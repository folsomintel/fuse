package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// hangingDestroyProvider wraps mockProvider so Destroy for one name blocks
// until its context ends or the test releases it, the way a host agent's
// DELETE queues behind a runaway exec holding the vm lock.
type hangingDestroyProvider struct {
	*mockProvider
	hangName string
	release  chan struct{}

	mu      sync.Mutex
	lastErr error
}

func newHangingDestroyProvider(t *testing.T, hangName string) *hangingDestroyProvider {
	p := &hangingDestroyProvider{
		mockProvider: newMockProvider(),
		hangName:     hangName,
		release:      make(chan struct{}),
	}
	t.Cleanup(func() { close(p.release) })
	return p
}

func (p *hangingDestroyProvider) Destroy(ctx context.Context, name string) error {
	if name != p.hangName {
		return p.mockProvider.Destroy(ctx, name)
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-p.release:
		err = errors.New("released by test cleanup")
	}
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
	return err
}

// drainingFleet builds a fleet with one Draining VM whose transition into
// draining is back-dated by drainingFor.
func drainingFleet(t *testing.T, cfg FleetConfig, taskID string, drainingFor time.Duration) (*FleetManager, string) {
	t.Helper()
	// keep the runtime ceiling out of the way; these tests are only
	// about the draining bound.
	if cfg.TaskStuckTimeout == 0 {
		cfg.TaskStuckTimeout = 24 * time.Hour
	}
	fm, vmID := fleetWithRunningVM(t, cfg, taskID, drainingFor)
	fm.mu.Lock()
	fm.vms[vmID].state = VMStateDraining
	fm.vms[vmID].updatedAt = time.Now().Add(-drainingFor)
	fm.mu.Unlock()
	return fm, vmID
}

func TestDrainingTimeoutDefaults(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, drainTimeout, drainTimeout / 2} {
		var buf bytes.Buffer
		fm := newInlineFleet(FleetConfig{
			Provider:        newMockProvider(),
			DrainingTimeout: d,
			Logger:          slog.New(slog.NewTextHandler(&buf, nil)),
		})
		if fm.drainingTimeout != DefaultDrainingTimeout {
			t.Errorf("DrainingTimeout %s: got %s, want the default %s", d, fm.drainingTimeout, DefaultDrainingTimeout)
		}
		// an unset timeout takes the default quietly; one that was asked
		// for and overridden is logged.
		warned := strings.Contains(buf.String(), "draining timeout at or below")
		if warned != (d != 0) {
			t.Errorf("DrainingTimeout %s: warned = %v, want %v (log: %q)", d, warned, d != 0, buf.String())
		}
	}
	fm := newInlineFleet(FleetConfig{Provider: newMockProvider(), DrainingTimeout: time.Hour})
	if fm.drainingTimeout != time.Hour {
		t.Errorf("explicit DrainingTimeout not kept: %s", fm.drainingTimeout)
	}
}

// an orphan destroy that hangs is cut off at the per-call timeout, counted as
// a failed attempt, and the cycle finishes.
func TestReconcileOrphan_hangingDestroyIsBounded(t *testing.T) {
	metrics := &captureMetrics{}
	p := newHangingDestroyProvider(t, "fuse-orphan")
	fm := newInlineFleet(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
		Metrics:  metrics,
	})
	fm.orphanDestroyTimeout = 50 * time.Millisecond

	p.mu.Lock()
	p.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan"}
	p.envs["fuse-orphan-2"] = &mockEnv{name: "fuse-orphan-2"}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		fm.reconcile(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile blocked on a hanging orphan destroy")
	}

	got := metrics.last()
	if got.OrphansFailed != 1 {
		t.Errorf("OrphansFailed = %d, want 1", got.OrphansFailed)
	}
	// the other orphan is still destroyed in the same cycle.
	if got.OrphansDestroyed != 1 {
		t.Errorf("OrphansDestroyed = %d, want 1", got.OrphansDestroyed)
	}
	p.mu.Lock()
	lastErr := p.lastErr
	p.mu.Unlock()
	if !errors.Is(lastErr, context.DeadlineExceeded) {
		t.Errorf("hanging destroy ended with %v, want context.DeadlineExceeded", lastErr)
	}
	fm.mu.RLock()
	retries := fm.orphanRetries["fuse-orphan"]
	fm.mu.RUnlock()
	if retries != 1 {
		t.Errorf("orphan retries = %d, want 1", retries)
	}
}
