package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

type fakeFleet struct {
	vms       []orchestrator.VMInfo
	hosts     []orchestrator.Host
	snapshots map[orchestrator.SnapshotCountKey]int
	snapErr   error
}

func (f fakeFleet) ListFleet() []orchestrator.VMInfo { return f.vms }
func (f fakeFleet) ListHosts() []orchestrator.Host   { return f.hosts }
func (f fakeFleet) SnapshotCounts(context.Context) (map[orchestrator.SnapshotCountKey]int, error) {
	return f.snapshots, f.snapErr
}

func TestFleetCollectorReportsLiveState(t *testing.T) {
	src := fakeFleet{
		vms: []orchestrator.VMInfo{
			{ID: "a", State: orchestrator.VMStateRunning, HostID: "h1"},
			{ID: "b", State: orchestrator.VMStateRunning, HostID: "h1"},
			{ID: "c", State: orchestrator.VMStateProvisioning, HostID: "h2"},
		},
		hosts: []orchestrator.Host{{
			ID: "h1", Region: "bhs", State: orchestrator.HostActive, LastSeen: time.Unix(1700000000, 0),
			Capacity:  orchestrator.HostCapacity{CPUs: 8, RamMB: 16384, StorageGB: 100, VMCount: 10},
			Allocated: orchestrator.HostCapacity{CPUs: 4, RamMB: 4096, StorageGB: 20, VMCount: 2},
		}},
		snapshots: map[orchestrator.SnapshotCountKey]int{
			{State: orchestrator.SnapshotStateReady, Mode: orchestrator.SnapshotModeManual}: 3,
		},
	}
	want := `
# HELP orchestrator_fleet_snapshots Snapshots by state and mode.
# TYPE orchestrator_fleet_snapshots gauge
orchestrator_fleet_snapshots{mode="manual",state="ready"} 3
# HELP orchestrator_fleet_vms VMs by state and host. host is empty for a single-provider fleet.
# TYPE orchestrator_fleet_vms gauge
orchestrator_fleet_vms{host="h1",state="running"} 2
orchestrator_fleet_vms{host="h2",state="provisioning"} 1
# HELP orchestrator_host_allocated Host capacity allocated to vms, by resource: cpus, ram_mb, storage_gb, vms, gpus.
# TYPE orchestrator_host_allocated gauge
orchestrator_host_allocated{host="h1",resource="cpus"} 4
orchestrator_host_allocated{host="h1",resource="gpus"} 0
orchestrator_host_allocated{host="h1",resource="ram_mb"} 4096
orchestrator_host_allocated{host="h1",resource="storage_gb"} 20
orchestrator_host_allocated{host="h1",resource="vms"} 2
# HELP orchestrator_host_info Always 1, one series per registered host, labeled with its state, region, backend and arch.
# TYPE orchestrator_host_info gauge
orchestrator_host_info{arch="amd64",backend="firecracker",host="h1",region="bhs",state="active"} 1
# HELP orchestrator_host_last_seen_timestamp_seconds Unix time the host agent last answered.
# TYPE orchestrator_host_last_seen_timestamp_seconds gauge
orchestrator_host_last_seen_timestamp_seconds{host="h1"} 1.7e+09
`
	c := NewFleetCollector(src)
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"orchestrator_fleet_snapshots", "orchestrator_fleet_vms", "orchestrator_host_allocated",
		"orchestrator_host_info", "orchestrator_host_last_seen_timestamp_seconds"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(c, "orchestrator_host_capacity"); n != 5 {
		t.Fatalf("capacity series = %d, want one per resource", n)
	}
}

// a store that cannot be read costs the snapshot series, not the scrape.
func TestFleetCollectorSurvivesASnapshotStoreError(t *testing.T) {
	c := NewFleetCollector(fakeFleet{
		vms:     []orchestrator.VMInfo{{ID: "a", State: orchestrator.VMStateRunning}},
		snapErr: errors.New("store down"),
	})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather failed: %v", err)
	}
	if n := testutil.CollectAndCount(c, "orchestrator_fleet_snapshots"); n != 0 {
		t.Fatalf("snapshot series = %d, want 0", n)
	}
}

func TestOperationCompletedCountsByResult(t *testing.T) {
	m := NewPrometheusMetrics(prometheus.NewRegistry())
	m.OperationCompleted("fork", 2*time.Second, nil)
	m.OperationCompleted("fork", time.Second, errors.New("boom"))
	if n := testutil.ToFloat64(m.operations.WithLabelValues("fork", "ok")); n != 1 {
		t.Fatalf("ok forks = %v", n)
	}
	if n := testutil.ToFloat64(m.operations.WithLabelValues("fork", "error")); n != 1 {
		t.Fatalf("failed forks = %v", n)
	}
	if n := testutil.CollectAndCount(m.operationDuration); n != 1 {
		t.Fatalf("duration series = %d, want 1", n)
	}
}
