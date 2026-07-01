package fusefile

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ResourceSpec mirrors internal/api.ResourceSpec field-for-field so the CLI can
// copy Compiled.Spec into the sdk/api spec without importing the api package.
type ResourceSpec struct {
	CPUs              int32
	RamMB             int32
	StorageGB         int32
	Region            string
	MaxRuntimeSeconds int64
}

// Compiled is the result of compiling a Fusefile. Spec is populated here;
// ManifestJSON, StartupScript and RequiredSecrets are populated in a later
// compile task and are left at their zero values by Compile.
type Compiled struct {
	Spec            ResourceSpec
	ManifestJSON    []byte   // populated in task 2.2
	StartupScript   string   // populated in task 2.2
	RequiredSecrets []string // populated in task 2.2
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

	return &Compiled{
		Spec: ResourceSpec{
			CPUs:  f.Resources.CPUs,
			RamMB: ramMB,
			// round up so any positive storage request is never silently zeroed
			// (e.g. "512MB" must provision 1GB, not floor to 0).
			StorageGB:         int32((int64(storageMB) + 1023) / 1024),
			MaxRuntimeSeconds: maxRuntimeSeconds,
		},
	}, nil
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
