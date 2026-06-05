package firecracker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/surf-dev/surf/apps/orchestrator/internal/core"
)

// TestSmoke exercises create/list/destroy against a live Firecracker agent.
// Skips automatically if FIRECRACKER_BASE_URL is not set. Keep the scope small
// to avoid relying on agent-side features beyond VM lifecycle.
func TestSmoke(t *testing.T) {
	baseURL := os.Getenv("FIRECRACKER_BASE_URL")
	token := os.Getenv("FIRECRACKER_TOKEN")
	if baseURL == "" {
		t.Skip("FIRECRACKER_BASE_URL not set; skipping live smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := "surf-smoke-" + strings.ReplaceAll(time.Now().Format("20060102-150405"), "-", "")
	cfg := Config{BaseURL: baseURL, Token: token}

	p := New(cfg)
	defer p.Close()

	spec := orchestrator.Spec{ // keep tiny
		Name:      name,
		CPUs:      1,
		RamMB:     512,
		StorageGB: 4,
	}

	env, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Ensure it shows up in list with the prefix.
	envs, err := p.List(ctx, name)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, e := range envs {
		if e.Name() == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("vm %s not found in list", name)
	}

	if err := p.Destroy(ctx, env.Name()); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}
