package orchestrator

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// opMetrics records each operation as "op:ok" or "op:error".
type opMetrics struct {
	mu  sync.Mutex
	ops []string
}

func (m *opMetrics) ReconcileCompleted(ReconcileSummary) {}

func (m *opMetrics) OperationCompleted(op string, _ time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.ops = append(m.ops, op+":"+result)
}

func TestLifecycleOperationsAreReported(t *testing.T) {
	metrics := &opMetrics{}
	fm := NewFleetManager(FleetConfig{Provider: newForkTestProvider(), Prefix: "fuse-", Metrics: metrics})
	ctx := context.Background()

	srcID := provisionSnapshotTestVM(t, fm, "task-1")
	forkID, err := fm.ForkEnvironment(ctx, srcID, ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := fm.DestroyVM(ctx, forkID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := fm.DestroyVM(ctx, "no-such-vm"); err == nil {
		t.Fatal("destroying an unknown vm succeeded")
	}

	// the fork's seed snapshot is reported too: it is a real checkpoint.
	want := []string{"create:ok", "snapshot:ok", "fork:ok", "destroy:ok", "destroy:error"}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if !slices.Equal(metrics.ops, want) {
		t.Fatalf("operations = %v, want %v", metrics.ops, want)
	}
}
