package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// a Fusefile with no resources block compiles to cpus/ram_mb/storage_gb 0.
// the create must still succeed, with the defaults filled in, rather than
// sending a 0 vcpu machine to the host.
func TestCreateEnvironment_bareFusefileGetsDefaultResources(t *testing.T) {
	f, err := fusefile.Parse([]byte("version: 1\nname: bare-test\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := fusefile.Compile(f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	h, _, _ := newTestHandler(t)
	rr := doJSON(t, mustRouter(t, h), http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "bare-test",
		Spec: ResourceSpec{
			CPUs:      c.Spec.CPUs,
			RamMB:     c.Spec.RamMB,
			StorageGB: c.Spec.StorageGB,
		},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Spec.CPUs != orchestrator.DefaultCPUs ||
		env.Spec.RamMB != orchestrator.DefaultRamMB ||
		env.Spec.StorageGB != orchestrator.DefaultStorageGB {
		t.Errorf("spec cpus/ram_mb/storage_gb = %d/%d/%d, want %d/%d/%d",
			env.Spec.CPUs, env.Spec.RamMB, env.Spec.StorageGB,
			orchestrator.DefaultCPUs, orchestrator.DefaultRamMB, orchestrator.DefaultStorageGB)
	}
}

func TestCreateEnvironment_negativeResourcesReturn400(t *testing.T) {
	for name, spec := range map[string]ResourceSpec{
		"cpus":       {CPUs: -1},
		"ram_mb":     {RamMB: -1},
		"storage_gb": {StorageGB: -1},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, _ := newTestHandler(t)
			rr := doJSON(t, mustRouter(t, h), http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
				TaskID:         "task-1",
				Spec:           spec,
				ManifestInline: encodeManifest(t),
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
			}
			if e := decodeError(t, rr.Body); e.Error.Code != CodeInvalidArgument {
				t.Errorf("code = %q, want %q", e.Error.Code, CodeInvalidArgument)
			}
		})
	}
}
