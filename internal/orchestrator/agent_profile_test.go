package orchestrator

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFuseComposePathConst(t *testing.T) {
	if fuseComposePath != "/fuse/compose.yaml" {
		t.Fatalf("fuseComposePath = %q, want /fuse/compose.yaml", fuseComposePath)
	}
}

func TestFusedAgentSpecWritesComposeWhenServicesDeclared(t *testing.T) {
	manifest := []byte(`{"version":"1","machine":{"workspace":"/workspace"},` +
		`"services":{"redis":{"image":"redis:7","ports":[6379],` +
		`"env":{"MODE":{"value":"prod"},"TOKEN":{"secret":"REDIS_TOKEN"}}}}}`)
	secretMap := map[string]string{"REDIS_TOKEN": "s3cr3t"}

	spec := FusedAgentSpec(manifest, secretMap, nil, BootOptions{})

	raw, ok := spec.Files[fuseComposePath]
	if !ok {
		t.Fatalf("expected a compose file at %s", fuseComposePath)
	}

	var proj struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Ports       []string          `yaml:"ports"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &proj); err != nil {
		t.Fatalf("compose is not valid yaml: %v", err)
	}
	svc, ok := proj.Services["redis"]
	if !ok {
		t.Fatalf("redis service missing from compose: %+v", proj)
	}
	if svc.Image != "redis:7" {
		t.Errorf("image = %q, want redis:7", svc.Image)
	}
	if len(svc.Ports) != 1 || svc.Ports[0] != "6379:6379" {
		t.Errorf("ports = %v, want [6379:6379]", svc.Ports)
	}
	if svc.Environment["MODE"] != "prod" {
		t.Errorf("env MODE = %q, want prod (literal value)", svc.Environment["MODE"])
	}
	if svc.Environment["TOKEN"] != "s3cr3t" {
		t.Errorf("env TOKEN = %q, want resolved secret s3cr3t", svc.Environment["TOKEN"])
	}
}

func TestFusedAgentSpecNoComposeWhenNoServices(t *testing.T) {
	spec := FusedAgentSpec(DefaultFusedManifest, nil, nil, BootOptions{})
	if _, ok := spec.Files[fuseComposePath]; ok {
		t.Fatal("did not expect a compose file for a manifest with no services")
	}
}

func TestComposeFromManifestDeterministic(t *testing.T) {
	// unsorted service and env keys must still yield byte-identical output.
	manifest := []byte(`{"services":{"b":{"image":"img-b",` +
		`"env":{"Z":{"value":"1"},"A":{"value":"2"}}},"a":{"image":"img-a"}}}`)

	out1, ok1 := composeFromManifest(manifest, nil)
	out2, ok2 := composeFromManifest(manifest, nil)
	if !ok1 || !ok2 {
		t.Fatal("expected services to be present")
	}
	if string(out1) != string(out2) {
		t.Errorf("composeFromManifest not deterministic:\n%s\n---\n%s", out1, out2)
	}
}
