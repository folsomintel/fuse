package api

import (
	"github.com/surf-dev/surf/apps/orchestrator/internal/core"
	"time"
)

// toAPIEnvironment converts an orchestrator VMInfo into the wire shape.
func toAPIEnvironment(v orchestrator.VMInfo) Environment {
	return Environment{
		ID:        v.ID,
		State:     string(v.State),
		TaskID:    v.TaskID,
		HostID:    v.HostID,
		URL:       v.URL,
		Spec:      toAPIResourceSpec(v.Spec),
		CreatedAt: v.CreatedAt,
		UpdatedAt: v.UpdatedAt,
		Error:     v.Error,
	}
}

// toAPIResourceSpec converts an orchestrator.Spec into the wire shape.
// The duration → seconds coercion here is one-way intentional: the
// wire contract uses seconds for ergonomics (no Go-specific parser).
func toAPIResourceSpec(s orchestrator.Spec) ResourceSpec {
	return ResourceSpec{
		CPUs:              int32(s.CPUs),
		RamMB:             int32(s.RamMB),
		StorageGB:         int32(s.StorageGB),
		Region:            s.Region,
		MaxRuntimeSeconds: int64(s.MaxRuntime.Seconds()),
	}
}

// toOrchestratorSpec converts a wire spec into the orchestrator type.
func toOrchestratorSpec(s ResourceSpec) orchestrator.Spec {
	return orchestrator.Spec{
		CPUs:       int(s.CPUs),
		RamMB:      int(s.RamMB),
		StorageGB:  int(s.StorageGB),
		Region:     s.Region,
		MaxRuntime: time.Duration(s.MaxRuntimeSeconds) * time.Second,
	}
}

// toAPIHost converts an orchestrator.Host into the wire shape.
func toAPIHost(h orchestrator.Host) HostInfo {
	return HostInfo{
		ID:     h.ID,
		URL:    h.URL,
		Region: h.Region,
		State:  string(h.State),
		Capacity: HostCapacity{
			CPUs:      h.Capacity.CPUs,
			RamMB:     h.Capacity.RamMB,
			StorageGB: h.Capacity.StorageGB,
			VMCount:   h.Capacity.VMCount,
		},
		Allocated: HostCapacity{
			CPUs:      h.Allocated.CPUs,
			RamMB:     h.Allocated.RamMB,
			StorageGB: h.Allocated.StorageGB,
			VMCount:   h.Allocated.VMCount,
		},
		LastSeen:  h.LastSeen,
		CreatedAt: h.CreatedAt,
		UpdatedAt: h.UpdatedAt,
	}
}

// toAPISnapshot converts a persisted SnapshotRecord into the wire shape.
// Any unknown metadata fields are dropped — the wire schema only
// exposes a subset today.
func toAPISnapshot(s orchestrator.SnapshotRecord) Snapshot {
	out := Snapshot{
		ID:               s.SnapshotID,
		VMID:             s.VMID,
		TaskID:           s.TaskID,
		TenantID:         s.TenantID,
		ParentSnapshotID: s.ParentSnapshotID,
		Mode:             string(s.Mode),
		State:            string(s.State),
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
		RetentionUntil:   s.RetentionUntil,
		SizeBytes:        s.SizeBytes,
		LastError:        s.LastError,
	}
	// Best-effort comment extraction. The orchestrator snapshot
	// metadata is a free-form JSON blob; we only surface a "comment"
	// field if it's present and a string. Missing or malformed
	// metadata falls through as empty.
	if len(s.Metadata) > 0 {
		if comment := extractStringField(s.Metadata, "comment"); comment != "" {
			out.Comment = comment
		}
		if exportRef := extractStringField(s.Metadata, "export_ref"); exportRef != "" {
			out.ExportRef = exportRef
		}
	}
	if len(s.Exports) > 0 {
		out.Exports = make([]SnapshotExport, 0, len(s.Exports))
		for _, export := range s.Exports {
			out.Exports = append(out.Exports, SnapshotExport{
				Destination: export.Destination,
				Status:      string(export.Status),
				RequestedAt: export.RequestedAt,
				UpdatedAt:   export.UpdatedAt,
				LastError:   export.LastError,
			})
		}
	}
	return out
}
