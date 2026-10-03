package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// FleetSource is the part of the fleet manager the collector reads.
type FleetSource interface {
	ListFleet() []orchestrator.VMInfo
	ListHosts() []orchestrator.Host
	SnapshotCounts(ctx context.Context) (map[orchestrator.SnapshotCountKey]int, error)
}

// FleetCollector reports the shape of the fleet at scrape time: vms by state
// and host, each host's capacity and allocation, and snapshots by state and
// mode. it reads live state on every scrape instead of keeping gauges, so a
// host or state that goes away simply stops being reported.
type FleetCollector struct {
	src FleetSource

	vms          *prometheus.Desc
	hostInfo     *prometheus.Desc
	hostLastSeen *prometheus.Desc
	hostCapacity *prometheus.Desc
	hostAlloc    *prometheus.Desc
	snapshots    *prometheus.Desc
}

var _ prometheus.Collector = (*FleetCollector)(nil)

// NewFleetCollector returns a collector over src. register it once.
func NewFleetCollector(src FleetSource) *FleetCollector {
	return &FleetCollector{
		src: src,
		vms: prometheus.NewDesc("orchestrator_fleet_vms",
			"VMs by state and host. host is empty for a single-provider fleet.",
			[]string{"state", "host"}, nil),
		hostInfo: prometheus.NewDesc("orchestrator_host_info",
			"Always 1, one series per registered host, labeled with its state, region, backend and arch.",
			[]string{"host", "state", "region", "backend", "arch"}, nil),
		hostLastSeen: prometheus.NewDesc("orchestrator_host_last_seen_timestamp_seconds",
			"Unix time the host agent last answered.",
			[]string{"host"}, nil),
		hostCapacity: prometheus.NewDesc("orchestrator_host_capacity",
			"Host capacity by resource: cpus, ram_mb, storage_gb, vms, gpus.",
			[]string{"host", "resource"}, nil),
		hostAlloc: prometheus.NewDesc("orchestrator_host_allocated",
			"Host capacity allocated to vms, by resource: cpus, ram_mb, storage_gb, vms, gpus.",
			[]string{"host", "resource"}, nil),
		snapshots: prometheus.NewDesc("orchestrator_fleet_snapshots",
			"Snapshots by state and mode.",
			[]string{"state", "mode"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *FleetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.vms
	ch <- c.hostInfo
	ch <- c.hostLastSeen
	ch <- c.hostCapacity
	ch <- c.hostAlloc
	ch <- c.snapshots
}

// Collect implements prometheus.Collector.
func (c *FleetCollector) Collect(ch chan<- prometheus.Metric) {
	type vmKey struct{ state, host string }
	vms := make(map[vmKey]int)
	for _, v := range c.src.ListFleet() {
		vms[vmKey{string(v.State), v.HostID}]++
	}
	for k, n := range vms {
		ch <- prometheus.MustNewConstMetric(c.vms, prometheus.GaugeValue, float64(n), k.state, k.host)
	}

	for _, h := range c.src.ListHosts() {
		arch := h.Capacity.Arch
		if arch == "" {
			arch = "amd64"
		}
		backend := string(h.Backend)
		if backend == "" {
			backend = string(orchestrator.BackendFirecracker)
		}
		ch <- prometheus.MustNewConstMetric(c.hostInfo, prometheus.GaugeValue, 1, h.ID, string(h.State), h.Region, backend, arch)
		if !h.LastSeen.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.hostLastSeen, prometheus.GaugeValue, float64(h.LastSeen.Unix()), h.ID)
		}
		for _, r := range []struct {
			name       string
			cap, alloc int
		}{
			{"cpus", h.Capacity.CPUs, h.Allocated.CPUs},
			{"ram_mb", h.Capacity.RamMB, h.Allocated.RamMB},
			{"storage_gb", h.Capacity.StorageGB, h.Allocated.StorageGB},
			{"vms", h.Capacity.VMCount, h.Allocated.VMCount},
			{"gpus", h.Capacity.GPUs, h.Allocated.GPUs},
		} {
			ch <- prometheus.MustNewConstMetric(c.hostCapacity, prometheus.GaugeValue, float64(r.cap), h.ID, r.name)
			ch <- prometheus.MustNewConstMetric(c.hostAlloc, prometheus.GaugeValue, float64(r.alloc), h.ID, r.name)
		}
	}

	// the snapshot count is the one read that leaves the process. when the
	// store is slow or down the scrape goes without it rather than failing
	// whole, which is what reporting an invalid metric would do.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	counts, err := c.src.SnapshotCounts(ctx)
	if err != nil {
		return
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.snapshots, prometheus.GaugeValue, float64(n), string(k.State), string(k.Mode))
	}
}
