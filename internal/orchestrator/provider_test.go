package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

// bootMockEnv implements Environment for boot testing.
type bootMockEnv struct {
	name         string
	url          string
	uploads      map[string][]byte
	agentCommand string
	execCalls    [][]string
	checkpoints  []Checkpoint
}

func (e *bootMockEnv) Name() string  { return e.name }
func (e *bootMockEnv) URL() string   { return e.url }
func (e *bootMockEnv) Token() string { return "" }
func (e *bootMockEnv) Exec(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, nil
}
func (e *bootMockEnv) ExecStream(_ context.Context, _, _ io.Writer, name string, args ...string) error {
	e.execCalls = append(e.execCalls, append([]string{name}, args...))
	return nil
}
func (e *bootMockEnv) Upload(_ context.Context, data []byte, path string) error {
	if e.uploads == nil {
		e.uploads = make(map[string][]byte)
	}
	e.uploads[path] = data
	return nil
}
func (e *bootMockEnv) StartAgent(_ context.Context, spec AgentSpec) error {
	e.agentCommand = spec.Command
	return nil
}
func (e *bootMockEnv) Checkpoint(_ context.Context, _ string) (string, error) {
	return "cp-1", nil
}
func (e *bootMockEnv) Restore(_ context.Context, _ string) error { return nil }
func (e *bootMockEnv) ListCheckpoints(_ context.Context) ([]Checkpoint, error) {
	return e.checkpoints, nil
}

// bootMockProvider implements Provider for boot testing.
type bootMockProvider struct {
	envs      map[string]*bootMockEnv
	createErr error
}

func newBootMockProvider() *bootMockProvider {
	return &bootMockProvider{envs: make(map[string]*bootMockEnv)}
}

func (p *bootMockProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	if p.createErr != nil {
		return nil, p.createErr
	}
	env := &bootMockEnv{name: spec.Name, url: "http://" + spec.Name}
	p.envs[spec.Name] = env
	return env, nil
}
func (p *bootMockProvider) Get(_ context.Context, name string) (Environment, error) {
	env, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return env, nil
}
func (p *bootMockProvider) Destroy(_ context.Context, name string) error {
	delete(p.envs, name)
	return nil
}
func (p *bootMockProvider) List(_ context.Context, _ string) ([]Environment, error) {
	var out []Environment
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}
func (p *bootMockProvider) Close() error { return nil }

func TestBoot_fresh_provision(t *testing.T) {
	p := newBootMockProvider()
	spec := Spec{Name: "test-vm", CPUs: 2, RamMB: 1024}
	manifest := []byte(`{"version":"1"}`)

	result, err := Boot(context.Background(), p, spec, manifest, nil, BootOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if result.Env == nil {
		t.Fatal("expected non-nil env")
	}
	if result.FromCache {
		t.Fatal("expected fresh provision, not from cache")
	}
	if result.BootTime <= 0 {
		t.Fatal("expected positive boot time")
	}

	// Verify the manifest declared by the fused profile was uploaded to the
	// profile-declared path.
	env := p.envs["test-vm"]
	if env.uploads[fuseManifestPath] == nil {
		t.Fatal("expected manifest upload")
	}
}

func TestBoot_with_secrets(t *testing.T) {
	p := newBootMockProvider()
	spec := Spec{Name: "test-vm"}
	manifest := []byte(`{"version":"1"}`)
	secrets := map[string]string{"KEY": "val"}

	result, err := Boot(context.Background(), p, spec, manifest, secrets, BootOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	env := result.Env.(*bootMockEnv)
	secretsData := env.uploads[fuseSecretsPath]
	if secretsData == nil {
		t.Fatal("expected secrets upload")
	}

	var decoded map[string]string
	json.Unmarshal(secretsData, &decoded)
	if decoded["KEY"] != "val" {
		t.Fatal("secrets content mismatch")
	}
}

func TestBoot_runs_startup_script(t *testing.T) {
	p := newBootMockProvider()
	spec := Spec{Name: "test-vm"}
	manifest := []byte(`{"version":"1"}`)

	result, err := Boot(context.Background(), p, spec, manifest, nil, BootOptions{StartupScript: "echo hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	env := result.Env.(*bootMockEnv)
	if len(env.execCalls) != 1 {
		t.Fatalf("expected one exec call, got %d", len(env.execCalls))
	}
	call := env.execCalls[0]
	if len(call) != 3 || call[0] != "sh" || call[1] != "-lc" || call[2] != "echo hello" {
		t.Fatalf("unexpected exec call: %#v", call)
	}
}

func TestBoot_from_checkpoint(t *testing.T) {
	p := newBootMockProvider()
	// Pre-create env with checkpoint.
	env := &bootMockEnv{
		name:        "test-vm",
		url:         "http://test-vm",
		checkpoints: []Checkpoint{{ID: "cp-1"}},
	}
	p.envs["test-vm"] = env

	spec := Spec{Name: "test-vm"}
	manifest := []byte(`{"version":"1"}`)

	result, err := Boot(context.Background(), p, spec, manifest, nil, BootOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !result.FromCache {
		t.Fatal("expected from cache when checkpoint exists")
	}
}

func TestBoot_create_failure(t *testing.T) {
	p := newBootMockProvider()
	p.createErr = fmt.Errorf("provider unavailable")

	spec := Spec{Name: "test-vm"}
	_, err := Boot(context.Background(), p, spec, []byte(`{}`), nil, BootOptions{}, nil)
	if err == nil {
		t.Fatal("expected error on create failure")
	}
}

func TestSpec_fields(t *testing.T) {
	s := Spec{
		Name:      "test",
		CPUs:      4,
		RamMB:     2048,
		StorageGB: 50,
		Region:    "us-east-1",
	}
	if s.Name != "test" {
		t.Fatal("name mismatch")
	}
	if s.CPUs != 4 {
		t.Fatal("CPUs mismatch")
	}
}

func TestCheckpoint_fields(t *testing.T) {
	cp := Checkpoint{
		ID:      "cp-123",
		Comment: "test checkpoint",
	}
	if cp.ID != "cp-123" {
		t.Fatal("ID mismatch")
	}
}
