// package fusefile is the canonical authoring format for a fuse environment.
// a Fusefile is parsed and compiled client-side into the orchestrator wire
// (CreateEnvironmentRequest); the orchestrator never sees a Fusefile.
package fusefile

// Fusefile is the v1 authoring contract.
type Fusefile struct {
	Version   int                `yaml:"version"`
	Image     string             `yaml:"image,omitempty"`
	Resources Resources          `yaml:"resources,omitempty"`
	Setup     []string           `yaml:"setup,omitempty"`
	Services  map[string]Service `yaml:"services,omitempty"`
	Run       string             `yaml:"run,omitempty"`
	Workspace string             `yaml:"workspace,omitempty"`
	Expose    []Expose           `yaml:"expose,omitempty"`
	Secrets   []string           `yaml:"secrets,omitempty"`
}

// Resources is the human-friendly hardware spec; compiled to ResourceSpec.
type Resources struct {
	CPUs    int32  `yaml:"cpus,omitempty"`
	GPU     int    `yaml:"gpu,omitempty"`      // device count: whole GPUs, or MIG instances when gpu_profile is set
	GPUKind string `yaml:"gpu_kind,omitempty"` // optional match, e.g. "a100"
	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// nvidia mig-parted vocabulary (e.g. "1g.10gb", "2g.20gb"). When set,
	// `gpu` counts MIG instances of this profile rather than whole
	// devices (decision D5). Empty means whole-device allocation.
	GPUProfile string `yaml:"gpu_profile,omitempty"`
	Memory     string `yaml:"memory,omitempty"`      // e.g. "2GB"
	Storage    string `yaml:"storage,omitempty"`     // e.g. "10GB"
	MaxRuntime string `yaml:"max_runtime,omitempty"` // go duration
}

// Service is one in-vm service; compiled to manifest.services and a compose unit.
type Service struct {
	Image string              `yaml:"image"`
	Ports []int               `yaml:"ports,omitempty"`
	Env   map[string]EnvValue `yaml:"env,omitempty"`
}

// EnvValue is either a literal value or a secret reference. exactly one is set.
type EnvValue struct {
	Value  string `yaml:"value,omitempty"`
	Secret string `yaml:"secret,omitempty"`
}

// Expose publishes a guest port to the outside world.
type Expose struct {
	Port int    `yaml:"port"`
	As   string `yaml:"as,omitempty"`
}
