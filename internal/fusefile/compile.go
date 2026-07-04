package fusefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ResourceSpec mirrors internal/api.ResourceSpec field-for-field so the CLI can
// copy Compiled.Spec into the sdk/api spec without importing the api package.
//
// Image selects the VM's base rootfs at create time (a name resolved by the
// firecracker host agent to a pre-baked rootfs file; see FUSEFILE_PLAN.md
// Phase 7). It lives here, not in the manifest json, because rootfs selection
// happens at Provider.Create — before the guest boots and long before the
// manifest is ever uploaded to it.
type ResourceSpec struct {
	CPUs              int32
	RamMB             int32
	StorageGB         int32
	Region            string
	MaxRuntimeSeconds int64
	Image             string
	GPUs              int32
	GPUKind           string
}

// ExposeSpec requests that a guest port be published as a reachable
// endpoint. Mirrors Fusefile's Expose entries one-for-one.
type ExposeSpec struct {
	Port int
	As   string
}

// Compiled is the result of compiling a Fusefile: the resource spec, the
// manifest json to upload to the guest, the startup script to run, the
// secrets the environment needs at create time, and any ports to expose.
type Compiled struct {
	Spec            ResourceSpec
	ManifestJSON    []byte
	StartupScript   string
	RequiredSecrets []string
	Expose          []ExposeSpec
}

// defaultWorkspace is used for manifest.machine.workspace when Fusefile.Workspace
// is unset.
const defaultWorkspace = "/workspace"

// manifest is the compiler-local marshal type for the guest-facing manifest
// json. it mirrors DefaultFusedManifest (internal/orchestrator/agent_profile.go)
// and the shape internal/secrets.ExtractRequiredSecrets reads; there is no
// shared Go struct for it since this package must not import either.
type manifest struct {
	Version  string                     `json:"version"`
	Machine  manifestMachine            `json:"machine"`
	Services map[string]manifestService `json:"services"`
}

type manifestMachine struct {
	Workspace string `json:"workspace"`
}

type manifestService struct {
	Image string                 `json:"image,omitempty"`
	Ports []int                  `json:"ports,omitempty"`
	Env   map[string]manifestEnv `json:"env,omitempty"`
}

type manifestEnv struct {
	Value  string `json:"value,omitempty"`
	Secret string `json:"secret,omitempty"`
}

// sizePattern matches an integer followed by an MB or GB suffix, e.g. "512MB"
// or "2GB". matching is done against an uppercased copy of the input so the
// unit is effectively case-insensitive.
var sizePattern = regexp.MustCompile(`^(\d+)(MB|GB)$`)

// Compile turns the human-friendly Fusefile.Resources into a ResourceSpec.
func Compile(f *Fusefile) (*Compiled, error) {
	var errs []error

	ramMB, err := parseSize(f.Resources.Memory)
	if err != nil {
		errs = append(errs, fmt.Errorf("resources.memory: %w", err))
	}

	storageMB, err := parseSize(f.Resources.Storage)
	if err != nil {
		errs = append(errs, fmt.Errorf("resources.storage: %w", err))
	}

	var maxRuntimeSeconds int64
	if f.Resources.MaxRuntime != "" {
		d, err := time.ParseDuration(f.Resources.MaxRuntime)
		if err != nil {
			errs = append(errs, fmt.Errorf("resources.max_runtime: %w", err))
		} else {
			maxRuntimeSeconds = int64(d.Seconds())
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	manifestJSON, requiredSecrets, err := compileManifest(f)
	if err != nil {
		return nil, err
	}

	return &Compiled{
		Spec: ResourceSpec{
			CPUs:  f.Resources.CPUs,
			RamMB: ramMB,
			// round up so any positive storage request is never silently zeroed
			// (e.g. "512MB" must provision 1GB, not floor to 0).
			StorageGB:         int32((int64(storageMB) + 1023) / 1024),
			MaxRuntimeSeconds: maxRuntimeSeconds,
			Image:             f.Image,
		},
		ManifestJSON:    manifestJSON,
		StartupScript:   compileStartupScript(f),
		RequiredSecrets: requiredSecrets,
		Expose:          compileExpose(f),
	}, nil
}

// compileExpose carries Fusefile.Expose through unchanged, one-for-one.
func compileExpose(f *Fusefile) []ExposeSpec {
	if len(f.Expose) == 0 {
		return nil
	}
	out := make([]ExposeSpec, len(f.Expose))
	for i, e := range f.Expose {
		out[i] = ExposeSpec(e)
	}
	return out
}

// compileManifest builds the guest-facing manifest json and the sorted,
// deduped union of secrets it references (plus f.Secrets).
func compileManifest(f *Fusefile) ([]byte, []string, error) {
	workspace := f.Workspace
	if workspace == "" {
		workspace = defaultWorkspace
	}

	secretSet := make(map[string]bool, len(f.Secrets))
	for _, s := range f.Secrets {
		secretSet[s] = true
	}

	m := manifest{
		Version:  "1",
		Machine:  manifestMachine{Workspace: workspace},
		Services: make(map[string]manifestService, len(f.Services)),
	}

	for name, svc := range f.Services {
		ms := manifestService{Image: svc.Image}
		if len(svc.Ports) > 0 {
			ms.Ports = append([]int(nil), svc.Ports...)
		}
		if len(svc.Env) > 0 {
			ms.Env = make(map[string]manifestEnv, len(svc.Env))
			for key, ev := range svc.Env {
				me := manifestEnv(ev)
				if ev.Secret != "" {
					secretSet[ev.Secret] = true
				}
				ms.Env[key] = me
			}
		}
		m.Services[name] = ms
	}

	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal manifest: %w", err)
	}

	var requiredSecrets []string
	for s := range secretSet {
		requiredSecrets = append(requiredSecrets, s)
	}
	sort.Strings(requiredSecrets)

	return manifestJSON, requiredSecrets, nil
}

// compileStartupScript joins setup lines and the run command into a single
// shell script with a strict-mode prelude. if there is nothing to run (no
// setup lines and no run command), it returns "" rather than a bare prelude.
func compileStartupScript(f *Fusefile) string {
	if len(f.Setup) == 0 && f.Run == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	for _, line := range f.Setup {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if f.Run != "" {
		b.WriteString(f.Run)
		b.WriteString("\n")
	}
	return b.String()
}

// parseSize parses a size string ("512MB", "2GB") into megabytes, base-1024.
// an empty string is not an error; it means the field was omitted and yields
// zero megabytes.
func parseSize(s string) (int32, error) {
	if s == "" {
		return 0, nil
	}

	m := sizePattern.FindStringSubmatch(strings.ToUpper(s))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}

	n, err := strconv.ParseInt(m[1], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}

	switch m[2] {
	case "MB":
		return int32(n), nil
	case "GB":
		// convert in int64 first; a well-formed but huge value (e.g.
		// "2097152GB") would otherwise overflow int32 silently and wrap
		// to a negative size.
		mb := n * 1024
		if mb > math.MaxInt32 {
			return 0, fmt.Errorf("invalid size %q: value too large", s)
		}
		return int32(mb), nil
	default:
		return 0, fmt.Errorf("invalid size %q", s)
	}
}
