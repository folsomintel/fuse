package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Load test defaults. Override via env vars:
//
//	LOAD_TEST_VMS=500        — total VMs to provision+destroy
//	LOAD_TEST_CONCURRENCY=50 — parallel goroutines
//
// Run: go test -run TestLoad -v -timeout 120s ./...
// Or:  LOAD_TEST_VMS=2000 LOAD_TEST_CONCURRENCY=100 go test -run TestLoad -v -timeout 300s ./...

func loadEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// TestLoad_provisionAndDestroy stress-tests concurrent VM provisioning
// and teardown through the FleetManager. It measures:
//   - Total wall-clock time for N provisions + N destroys
//   - Per-VM provision and destroy latency (p50/p99)
//   - Error rates under contention
//   - Correct cleanup (fleet empty at end)
//
// Uses the stub provider (instant Create/Destroy) so the test isolates
// FleetManager lock contention, state store throughput, and scheduling
// overhead — not provider I/O.
func TestLoad_provisionAndDestroy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	totalVMs := loadEnvInt("LOAD_TEST_VMS", 500)
	concurrency := loadEnvInt("LOAD_TEST_CONCURRENCY", 50)

	t.Logf("load test: %d VMs, %d concurrent workers", totalVMs, concurrency)

	provider := newStubProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "load-",
	})

	ctx := context.Background()
	manifest := []byte(`{"version":"1","services":{}}`)

	// ── Phase 1: Provision ──────────────────────────────────────────

	provisionLatencies := make([]time.Duration, totalVMs)
	var provisionErrors atomic.Int64

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	provisionStart := time.Now()
	for i := 0; i < totalVMs; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			taskID := fmt.Sprintf("task-%06d", idx)
			start := time.Now()
			_, err := fm.ProvisionAndAssign(ctx, taskID, Spec{CPUs: 1, RamMB: 256}, manifest, nil, BootOptions{})
			provisionLatencies[idx] = time.Since(start)

			if err != nil {
				provisionErrors.Add(1)
				if idx < 5 { // log first few errors
					t.Logf("provision error [%d]: %v", idx, err)
				}
			}
		}(i)
	}
	wg.Wait()
	provisionDuration := time.Since(provisionStart)

	errCount := provisionErrors.Load()
	successCount := int64(totalVMs) - errCount
	t.Logf("provision: %d/%d succeeded in %s (%.1f VMs/sec)",
		successCount, totalVMs, provisionDuration, float64(successCount)/provisionDuration.Seconds())

	if errCount > 0 {
		t.Logf("provision: %d errors", errCount)
	}

	// Verify fleet size matches successes.
	fleet := fm.ListFleet()
	if len(fleet) != int(successCount) {
		t.Errorf("fleet size = %d, want %d", len(fleet), successCount)
	}

	logLatencyStats(t, "provision", provisionLatencies[:successCount])

	// ── Phase 2: Destroy ────────────────────────────────────────────

	destroyLatencies := make([]time.Duration, 0, len(fleet))
	var destroyMu sync.Mutex
	var destroyErrors atomic.Int64

	destroyStart := time.Now()
	for i, info := range fleet {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, vmID string) {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()
			err := fm.DestroyVM(ctx, vmID)
			lat := time.Since(start)

			destroyMu.Lock()
			destroyLatencies = append(destroyLatencies, lat)
			destroyMu.Unlock()

			if err != nil {
				destroyErrors.Add(1)
				if idx < 5 {
					t.Logf("destroy error [%d]: %v", idx, err)
				}
			}
		}(i, info.ID)
	}
	wg.Wait()
	destroyDuration := time.Since(destroyStart)

	destroyErrs := destroyErrors.Load()
	destroySuccess := int64(len(fleet)) - destroyErrs
	t.Logf("destroy: %d/%d succeeded in %s (%.1f VMs/sec)",
		destroySuccess, len(fleet), destroyDuration, float64(destroySuccess)/destroyDuration.Seconds())

	if destroyErrs > 0 {
		t.Errorf("destroy: %d errors", destroyErrs)
	}

	logLatencyStats(t, "destroy", destroyLatencies)

	// ── Phase 3: Verify cleanup ─────────────────────────────────────

	remaining := fm.ListFleet()
	if len(remaining) != 0 {
		t.Errorf("fleet not empty after destroy: %d VMs remaining", len(remaining))
	}

	// Total throughput.
	totalDuration := provisionDuration + destroyDuration
	t.Logf("total: %d provision + %d destroy in %s",
		totalVMs, len(fleet), totalDuration)
}

// TestLoad_burstProvision fires all provisions simultaneously (no
// semaphore throttling) to find the breaking point under maximum lock
// contention.
func TestLoad_burstProvision(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	totalVMs := loadEnvInt("LOAD_TEST_VMS", 200)
	t.Logf("burst test: %d VMs, all concurrent", totalVMs)

	provider := newStubProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "burst-",
	})

	ctx := context.Background()
	manifest := []byte(`{"version":"1","services":{}}`)

	var wg sync.WaitGroup
	var errors atomic.Int64

	start := time.Now()
	for i := 0; i < totalVMs; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("burst-%06d", idx)
			if _, err := fm.ProvisionAndAssign(ctx, taskID, Spec{CPUs: 1, RamMB: 256}, manifest, nil, BootOptions{}); err != nil {
				errors.Add(1)
			}
		}(i)
	}
	wg.Wait()
	duration := time.Since(start)

	errCount := errors.Load()
	successCount := int64(totalVMs) - errCount
	t.Logf("burst provision: %d/%d in %s (%.1f VMs/sec)",
		successCount, totalVMs, duration, float64(successCount)/duration.Seconds())

	fleet := fm.ListFleet()
	if len(fleet) != int(successCount) {
		t.Errorf("fleet size = %d, want %d", len(fleet), successCount)
	}

	// Destroy all.
	destroyStart := time.Now()
	for _, info := range fleet {
		wg.Add(1)
		go func(vmID string) {
			defer wg.Done()
			_ = fm.DestroyVM(ctx, vmID)
		}(info.ID)
	}
	wg.Wait()
	t.Logf("burst destroy: %d in %s", len(fleet), time.Since(destroyStart))

	if remaining := fm.ListFleet(); len(remaining) != 0 {
		t.Errorf("%d VMs remaining after destroy", len(remaining))
	}
}

// TestLoad_provisionWithHosts tests scheduling under load with
// multiple registered hosts and capacity constraints.
func TestLoad_provisionWithHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	totalVMs := loadEnvInt("LOAD_TEST_VMS", 200)
	concurrency := loadEnvInt("LOAD_TEST_CONCURRENCY", 50)
	numHosts := 5

	t.Logf("host scheduling test: %d VMs across %d hosts, %d concurrent", totalVMs, numHosts, concurrency)

	stubProv := newStubProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: stubProv,
		Prefix:   "sched-",
	})

	ctx := context.Background()

	// Register hosts with limited capacity.
	vmsPerHost := (totalVMs / numHosts) + 10 // some headroom
	for i := 0; i < numHosts; i++ {
		host := Host{
			ID:  fmt.Sprintf("host-%d", i),
			URL: fmt.Sprintf("http://host-%d.test", i),
			Capacity: HostCapacity{
				CPUs:      1000,
				RamMB:     256 * vmsPerHost,
				StorageGB: 1000,
				VMCount:   vmsPerHost,
			},
		}
		if err := fm.RegisterHost(ctx, host, stubProv); err != nil {
			t.Fatalf("register host %d: %v", i, err)
		}
	}

	manifest := []byte(`{"version":"1","services":{}}`)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var errors atomic.Int64
	hostCounts := make(map[string]int)
	var hostMu sync.Mutex

	start := time.Now()
	for i := 0; i < totalVMs; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			taskID := fmt.Sprintf("sched-%06d", idx)
			info, err := fm.ProvisionAndAssign(ctx, taskID, Spec{CPUs: 1, RamMB: 256}, manifest, nil, BootOptions{})
			if err != nil {
				errors.Add(1)
				return
			}
			hostMu.Lock()
			hostCounts[info.HostID]++
			hostMu.Unlock()
		}(i)
	}
	wg.Wait()
	duration := time.Since(start)

	errCount := errors.Load()
	successCount := int64(totalVMs) - errCount
	t.Logf("scheduled provision: %d/%d in %s (%.1f VMs/sec)",
		successCount, totalVMs, duration, float64(successCount)/duration.Seconds())

	// Log distribution across hosts.
	hostMu.Lock()
	for hostID, count := range hostCounts {
		t.Logf("  %s: %d VMs", hostID, count)
	}
	hostMu.Unlock()

	if errCount > 0 {
		t.Logf("errors: %d (may be expected if capacity exhausted)", errCount)
	}

	// Destroy all.
	fleet := fm.ListFleet()
	destroyStart := time.Now()
	for _, info := range fleet {
		wg.Add(1)
		go func(vmID string) {
			defer wg.Done()
			_ = fm.DestroyVM(ctx, vmID)
		}(info.ID)
	}
	wg.Wait()
	t.Logf("destroy: %d in %s", len(fleet), time.Since(destroyStart))

	if remaining := fm.ListFleet(); len(remaining) != 0 {
		t.Errorf("%d VMs remaining", len(remaining))
	}
}

// logLatencyStats prints p50, p90, p99, and max latencies.
func logLatencyStats(t *testing.T, label string, latencies []time.Duration) {
	t.Helper()
	if len(latencies) == 0 {
		return
	}

	// Sort for percentiles. Use insertion sort since we only need
	// a few percentile values and the slice is already mostly ordered
	// from concurrent goroutines finishing in roughly submission order.
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	// Simple sort — stdlib sort.Slice would work too but this avoids
	// the import for a test helper.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	n := len(sorted)
	t.Logf("%s latency: p50=%s p90=%s p99=%s max=%s",
		label,
		sorted[n/2],
		sorted[n*90/100],
		sorted[n*99/100],
		sorted[n-1],
	)
}

// newStubProvider creates a minimal in-memory provider for load tests.
// Identical to the firecracker stub but lives here to avoid an import
// cycle and to keep the load test self-contained.
func newStubProvider() *stubLoadProvider {
	return &stubLoadProvider{envs: make(map[string]*stubLoadEnv)}
}

type stubLoadProvider struct {
	mu   sync.Mutex
	envs map[string]*stubLoadEnv
}

func (p *stubLoadProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := &stubLoadEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *stubLoadProvider) Get(_ context.Context, name string) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("not found: %s", name)
	}
	return e, nil
}

func (p *stubLoadProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, name)
	return nil
}

func (p *stubLoadProvider) List(_ context.Context, _ string) ([]Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (*stubLoadProvider) Close() error { return nil }

type stubLoadEnv struct {
	name string
	url  string
}

func (e *stubLoadEnv) Name() string  { return e.name }
func (e *stubLoadEnv) URL() string   { return e.url }
func (e *stubLoadEnv) Token() string { return "" }

func (e *stubLoadEnv) Exec(context.Context, []string, ExecOptions) (ExecResult, error) {
	return ExecResult{}, nil
}
func (e *stubLoadEnv) ExecStream(_ context.Context, _ io.Writer, _ io.Writer, _ string, _ ...string) error {
	return nil
}
func (e *stubLoadEnv) Upload(context.Context, []byte, string) error          { return nil }
func (e *stubLoadEnv) StartAgent(context.Context, AgentSpec) error           { return nil }
func (e *stubLoadEnv) Checkpoint(context.Context, string) (string, error)    { return "cp-1", nil }
func (e *stubLoadEnv) Restore(context.Context, string) error                 { return nil }
func (e *stubLoadEnv) ListCheckpoints(context.Context) ([]Checkpoint, error) { return nil, nil }
