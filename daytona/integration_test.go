//go:build integration

// Live Daytona integration test. Skipped unless built with -tags=integration
// AND DAYTONA_API_KEY is set in the environment.
//
// What it covers (Workstream B8):
//   - Provider.Create against real Daytona.
//   - Environment.Upload writes a small file via single-file endpoint.
//   - Environment.Exec reads it back.
//   - Provider.Destroy idempotently tears down.
//
// What it deliberately does NOT cover:
//   - StartAgent + gRPC-Web reachability — that's a follow-up smoke once
//     the surfd-latest GitHub release publishes the gRPC-Web binary.
//
// Run:
//   set -a && source ../../../.env && set +a
//   cd apps/orchestrator/daytona
//   go test -tags=integration -run TestIntegration_Daytona -v ./...
//
// Live API + nonzero cost — only runs when explicitly opted in via tag.

package daytona

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/surf-dev/surf/apps/orchestrator"
)

func TestIntegration_Daytona_RoundTrip(t *testing.T) {
	apiKey := os.Getenv("DAYTONA_API_KEY")
	if apiKey == "" {
		t.Skip("DAYTONA_API_KEY unset; skipping live Daytona integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	p := New(Config{
		BaseURL: os.Getenv("DAYTONA_BASE_URL"),
		APIKey:  apiKey,
	})
	defer p.Close()

	specName := "surf-int-test-" + time.Now().UTC().Format("20060102-150405")
	t.Logf("creating sandbox name=%s", specName)

	env, err := p.Create(ctx, orchestrator.Spec{Name: specName})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Always tear down, even on failure.
	t.Cleanup(func() {
		teardownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.Destroy(teardownCtx, specName); err != nil {
			t.Logf("Destroy: %v (sandbox id=%s may need manual cleanup)", err, env.Name())
		}
	})

	if env.URL() == "" {
		t.Errorf("expected preview URL, got empty")
	}
	if env.Token() == "" {
		t.Errorf("expected preview token, got empty")
	}

	// Upload a small file under /home/daytona (no /surf prep needed).
	const path = "/home/daytona/surf-int-test.txt"
	const body = "hello-from-int-test\n"
	if err := env.Upload(ctx, []byte(body), path); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	out, err := env.Exec(ctx, "cat", path)
	if err != nil {
		t.Fatalf("Exec cat: %v (output=%q)", err, out)
	}
	if !strings.Contains(string(out), "hello-from-int-test") {
		t.Errorf("Exec output did not contain expected payload: %q", out)
	}

	// Exec a no-op (proves session-less /process/execute works).
	if _, err := env.Exec(ctx, "true"); err != nil {
		t.Errorf("Exec true: %v", err)
	}

	t.Logf("integration round-trip OK; cleanup will Destroy sandbox")

	// Idempotent destroy verification: Destroy twice in a row.
	if err := p.Destroy(ctx, specName); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	if err := p.Destroy(ctx, specName); err != nil {
		t.Fatalf("second Destroy (must be idempotent): %v", err)
	}
}
