package fusefile

import (
	"strings"
	"testing"
)

func TestCompileMemoryGB(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{Memory: "2GB"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.RamMB != 2048 {
		t.Fatalf("ram_mb = %d, want 2048", c.Spec.RamMB)
	}
}

func TestCompileMemoryMB(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{Memory: "512MB"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.RamMB != 512 {
		t.Fatalf("ram_mb = %d, want 512", c.Spec.RamMB)
	}
}

func TestCompileStorageGB(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{Storage: "10GB"}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.StorageGB != 10 {
		t.Fatalf("storage_gb = %d, want 10", c.Spec.StorageGB)
	}
}

func TestCompileStorageRoundsUp(t *testing.T) {
	// sub-1GB (and non-whole-GB) storage must round up, never floor to a
	// smaller value than requested.
	cases := []struct {
		name  string
		input string
		want  int32
	}{
		{"sub gb rounds up to 1", "512MB", 1},
		{"non whole gb rounds up", "1536MB", 2},
		{"whole gb unchanged", "10GB", 10},
		{"int32 max storage rounds up positive", "2147483647MB", 2097152},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: Resources{Storage: tc.input}}
			c, err := Compile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.Spec.StorageGB != tc.want {
				t.Fatalf("storage_gb = %d, want %d", c.Spec.StorageGB, tc.want)
			}
		})
	}
}

func TestCompileMaxRuntime(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int64
	}{
		{"one hour", "1h", 3600},
		{"ninety minutes", "90m", 5400},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: Resources{MaxRuntime: tc.input}}
			c, err := Compile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.Spec.MaxRuntimeSeconds != tc.want {
				t.Fatalf("max_runtime_seconds = %d, want %d", c.Spec.MaxRuntimeSeconds, tc.want)
			}
		})
	}
}

func TestCompileCPUsPassthrough(t *testing.T) {
	f := &Fusefile{Version: 1, Resources: Resources{CPUs: 4}}
	c, err := Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.CPUs != 4 {
		t.Fatalf("cpus = %d, want 4", c.Spec.CPUs)
	}
}

func TestCompileEmptyResources(t *testing.T) {
	f := &Fusefile{Version: 1}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Spec.CPUs != 0 || c.Spec.RamMB != 0 || c.Spec.StorageGB != 0 || c.Spec.MaxRuntimeSeconds != 0 || c.Spec.Region != "" {
		t.Fatalf("expected zero spec, got %+v", c.Spec)
	}
}

func TestCompileInvalid(t *testing.T) {
	cases := []struct {
		name        string
		resources   Resources
		wantContain string
	}{
		{
			name:        "invalid memory unit words",
			resources:   Resources{Memory: "2 gigabytes"},
			wantContain: `resources.memory: invalid size "2 gigabytes"`,
		},
		{
			name:        "memory missing unit",
			resources:   Resources{Memory: "2"},
			wantContain: `resources.memory: invalid size "2"`,
		},
		{
			name:        "memory missing number",
			resources:   Resources{Memory: "GB"},
			wantContain: `resources.memory: invalid size "GB"`,
		},
		{
			name:        "memory unknown unit",
			resources:   Resources{Memory: "2TB"},
			wantContain: `resources.memory: invalid size "2TB"`,
		},
		{
			name:        "invalid storage",
			resources:   Resources{Storage: "10 gigs"},
			wantContain: `resources.storage: invalid size "10 gigs"`,
		},
		{
			name:        "memory gb value overflows int32",
			resources:   Resources{Memory: "2097152GB"},
			wantContain: `resources.memory: invalid size "2097152GB": value too large`,
		},
		{
			// the underlying time.ParseDuration error is wrapped with %w, so
			// only assert the field path prefix rather than the exact
			// stdlib wording.
			name:        "invalid duration",
			resources:   Resources{MaxRuntime: "1 hour"},
			wantContain: `resources.max_runtime: `,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fusefile{Version: 1, Resources: tc.resources}
			_, err := Compile(f)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContain)
			}
		})
	}
}

// task 2.2 populates ManifestJSON, StartupScript and RequiredSecrets; this
// task must leave them at their zero values.
func TestCompileLeavesTask22FieldsZero(t *testing.T) {
	f := &Fusefile{Version: 1}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.ManifestJSON != nil {
		t.Errorf("ManifestJSON: got %v, want nil (populated in task 2.2)", c.ManifestJSON)
	}
	if c.StartupScript != "" {
		t.Errorf("StartupScript: got %q, want empty (populated in task 2.2)", c.StartupScript)
	}
	if c.RequiredSecrets != nil {
		t.Errorf("RequiredSecrets: got %v, want nil (populated in task 2.2)", c.RequiredSecrets)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantMB  int32
		wantErr bool
	}{
		{"empty is not an error", "", 0, false},
		{"megabytes", "512MB", 512, false},
		{"gigabytes", "2GB", 2048, false},
		{"lowercase unit", "2gb", 2048, false},
		{"words instead of unit", "2 gigabytes", 0, true},
		{"missing unit", "2", 0, true},
		{"missing number", "GB", 0, true},
		{"unknown unit", "2TB", 0, true},
		{"large but valid gigabytes", "3000GB", 3072000, false},
		{"gigabytes overflows int32", "2097152GB", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mb, err := parseSize(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mb != tc.wantMB {
				t.Fatalf("mb = %d, want %d", mb, tc.wantMB)
			}
		})
	}
}
