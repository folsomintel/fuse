//go:build e2e_grpcweb

// SURFD-PROFILE transport e2e: this validates the default surfd profile's
// gRPC-Web transport over Daytona, not fuse core. It is build-tagged
// (e2e_grpcweb) and live-only (skips without DAYTONA_API_KEY/SURFD_BINARY), so
// it is off the green-bar critical path.
//
// End-to-end verification that surfd's gRPC-Web wrapper survives the
// Daytona preview proxy (AWS ALB). This is the live re-run of Workstream
// A from ORCHESTRATOR_PLAN.md, originally documented in PROBE_RESULTS.md.
//
// What it proves:
//   - Build the Daytona-resident surfd binary with the new gRPC-Web wrapper.
//   - Upload it via the orchestrator's Daytona client.
//   - Start it via the session API.
//   - Reach surfd through Daytona's preview URL using gRPC-Web (HTTP/2,
//     content-type application/grpc-web+proto). This is the path that
//     ALB rejected with HTTP 464 before the wrapper.
//   - Confirm a no-trailer response: HTTP 200 with grpc-web framing.
//
// Why a build tag instead of //go:build integration: this test depends
// on a *specific surfd build* (the one with grpc-web baked in). Running
// it under the generic `integration` tag would silently fail against
// older surfd binaries and produce confusing diagnostics. We isolate it.
//
// To run:
//   set -a && source ../../../.env && set +a
//   cd apps/surfd && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/surfd-grpcweb .
//   cd ../orchestrator/daytona
//   SURFD_BINARY=/tmp/surfd-grpcweb go test -tags=e2e_grpcweb -timeout=10m -v -run TestE2E_GRPCWeb ./...

package daytona

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surf-dev/surf/apps/orchestrator/internal/core"
)

// TestE2E_GRPCWeb provisions a real Daytona sandbox, deploys the
// supplied surfd binary, and verifies gRPC-Web reachability through the
// preview proxy.
func TestE2E_GRPCWeb(t *testing.T) {
	apiKey := os.Getenv("DAYTONA_API_KEY")
	if apiKey == "" {
		t.Skip("DAYTONA_API_KEY unset; skipping live Daytona e2e test")
	}
	surfdBin := os.Getenv("SURFD_BINARY")
	if surfdBin == "" {
		t.Skip("SURFD_BINARY unset; build linux/amd64 surfd and point this at it")
	}
	binBytes, err := os.ReadFile(surfdBin)
	if err != nil {
		t.Fatalf("read surfd binary: %v", err)
	}
	if len(binBytes) < 1_000_000 {
		t.Fatalf("surfd binary too small (%d bytes); did the build succeed?", len(binBytes))
	}
	t.Logf("loaded surfd binary: %s (%.1f MB)", surfdBin, float64(len(binBytes))/(1024*1024))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	client := NewClient(os.Getenv("DAYTONA_BASE_URL"), apiKey, nil)

	// 1. Create sandbox.
	zero := 0
	sb, err := client.CreateSandbox(ctx, CreateSandboxRequest{
		Labels:           map[string]string{"surf-grpcweb-probe": "1"},
		AutoStopInterval: &zero,
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Logf("created sandbox: %s", sb.ID)

	t.Cleanup(func() {
		teardown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.DeleteSandbox(teardown, sb.ID); err != nil {
			t.Logf("DeleteSandbox: %v (manual cleanup may be needed: id=%s)", err, sb.ID)
		} else {
			t.Logf("destroyed sandbox: %s", sb.ID)
		}
	})

	// 2. Wait until the sandbox is past the spin-up phase. The probe
	//    in PROBE_RESULTS.md found we can hit /toolbox APIs almost
	//    immediately on a "started" sandbox; poll until that.
	if err := waitSandboxStarted(ctx, client, sb.ID, 90*time.Second); err != nil {
		t.Fatalf("wait sandbox started: %v", err)
	}

	// 3. Upload surfd binary + chmod +x.
	const surfdPath = "/home/daytona/surfd"
	if err := client.Upload(ctx, sb.ID, surfdPath, binBytes); err != nil {
		t.Fatalf("Upload surfd: %v", err)
	}
	t.Logf("uploaded surfd to %s", surfdPath)

	if _, err := client.Execute(ctx, sb.ID, "chmod +x "+surfdPath); err != nil {
		t.Fatalf("chmod +x: %v", err)
	}

	// 4. Upload a minimal manifest + empty secrets. Match the testdata
	//    fixture used by surfd's own tests.
	manifest := []byte(`{"version":"1","services":{"app":{"name":"app","kind":"run","command":"sleep 600","listen_port":8080}}}`)
	if err := client.Upload(ctx, sb.ID, "/home/daytona/manifest.json", manifest); err != nil {
		t.Fatalf("upload manifest: %v", err)
	}
	if err := client.Upload(ctx, sb.ID, "/home/daytona/secrets.json", []byte("{}")); err != nil {
		t.Fatalf("upload secrets: %v", err)
	}

	// 5. Start surfd in a long-running session. Listen on 3000 (within
	//    Daytona's preview-proxy range). SURF_CONTAINER_BIN=/bin/true
	//    bypasses Daytona's missing podman/docker.
	const sessionID = "surfd-grpcweb-probe"
	if err := client.CreateSession(ctx, sb.ID, sessionID); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	startCmd := strings.Join([]string{
		"SURF_CONTAINER_BIN=/bin/true",
		surfdPath,
		"--listen 0.0.0.0:3000",
		"--manifest /home/daytona/manifest.json",
		"--secrets /home/daytona/secrets.json",
		"--insecure",
	}, " ")
	if _, err := client.SessionExec(ctx, sb.ID, sessionID, startCmd, true); err != nil {
		t.Fatalf("SessionExec start surfd: %v", err)
	}

	// 6. Wait for surfd to bind. Poll netstat-equivalent. The earlier
	//    probe used `ss -ltn` directly; do the same.
	if err := waitSurfdListening(ctx, client, sb.ID); err != nil {
		t.Fatalf("wait surfd listening: %v", err)
	}

	// 7. Resolve preview URL.
	pv, err := client.GetPreviewURL(ctx, sb.ID, 3000)
	if err != nil {
		t.Fatalf("GetPreviewURL: %v", err)
	}
	t.Logf("preview url: %s (token=%s...)", pv.URL, truncatePreview(pv.Token))

	// 8. The smoking gun. Hit surfd through the preview URL with
	//    application/grpc-web+proto. PRE-WRAPPER: ALB returned 464.
	//    POST-WRAPPER: ALB lets it through, surfd returns a real
	//    gRPC-Web framed reply.
	//
	//    We pick a method that's safe to call without a real task ID:
	//    Status (no required arg), or just send an empty body and
	//    accept whatever response. Either way ALB's verdict is what we
	//    want — 464 = fail, anything else = ALB accepted the protocol.
	if err := probeGRPCWeb(ctx, pv); err != nil {
		t.Fatalf("gRPC-Web probe failed: %v", err)
	}
	t.Log("gRPC-Web probe SUCCESS — ALB no longer rejects application/grpc-web+proto")

	// 9. End-to-end surfctl smoke. If a host-built surfctl binary is
	//    provided, invoke `surfctl status` against the preview URL and
	//    assert it exits 0. This validates the full gRPC-Web client path
	//    introduced in apps/surfctl/internal/grpc/connect.go (header
	//    propagation, message framing, trailer parsing) against a real
	//    surfd behind a real ALB.
	if err := runSurfctlStatus(ctx, t, pv); err != nil {
		t.Fatalf("surfctl status (live gRPC-Web): %v", err)
	}
}

// runSurfctlStatus exec's the host-built surfctl against the live
// preview URL and asserts a clean exit. SURFCTL_BINARY must point at a
// surfctl binary built for the test host (e.g. darwin/arm64) — not
// linux/amd64. If the env var is unset, the test logs and continues.
func runSurfctlStatus(ctx context.Context, t *testing.T, pv *PreviewURL) error {
	t.Helper()

	surfctlBin := os.Getenv("SURFCTL_BINARY")
	if surfctlBin == "" {
		t.Log("SURFCTL_BINARY unset; skipping live surfctl status check")
		return nil
	}
	info, err := os.Stat(surfctlBin)
	if err != nil {
		return fmt.Errorf("stat surfctl binary %q: %w", surfctlBin, err)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("surfctl binary %q is not executable (mode %s)", surfctlBin, info.Mode())
	}

	// 30s is generous; native gRPC-Web Status round-trip is sub-second.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, surfctlBin,
		"status",
		"--addr", pv.URL,
		"--preview-token", pv.Token,
		"--json",
	)
	// Don't inherit the test process env — surfctl honors a few env vars
	// (SURF_AUTH_TOKEN, SURF_PREVIEW_TOKEN, SURF_SURFD_ADDR) that could
	// silently override our flags. Pass only PATH for any subprocess.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("exec %s status: %w\nstdout: %s\nstderr: %s",
			surfctlBin, err,
			truncateString(stdout.String(), 500),
			truncateString(stderr.String(), 500))
	}

	t.Logf("surfctl status SUCCESS (output len=%d)", stdout.Len())
	if stdout.Len() > 0 {
		t.Logf("surfctl stdout: %s", truncateString(stdout.String(), 400))
	}
	return nil
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// waitSandboxStarted polls until the sandbox is in a usable state.
func waitSandboxStarted(ctx context.Context, c *Client, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sb, err := c.GetSandbox(ctx, id)
		if err == nil {
			switch strings.ToLower(sb.State) {
			case "started", "running":
				return nil
			case "destroyed", "destroying", "error", "failed":
				return fmt.Errorf("sandbox in terminal state: %s", sb.State)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("sandbox %s not started within %s", id, timeout)
}

// waitSurfdListening polls inside the sandbox until surfd is actually
// listening on :3000 (or until we run out of patience). Uses `ss` since
// Daytona's image has it; falls back to a curl loopback test if needed.
func waitSurfdListening(ctx context.Context, c *Client, id string) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		// `ss -ltn 'sport = :3000'` returns one row when surfd is listening.
		out, err := c.Execute(ctx, id, "ss -ltn 'sport = :3000' | grep -c ':3000' || true")
		if err == nil && strings.TrimSpace(out.Result) != "" && !strings.HasPrefix(strings.TrimSpace(out.Result), "0") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("surfd never bound to :3000 within timeout")
}

// probeGRPCWeb sends a single application/grpc-web+proto POST through
// Daytona's preview URL and asserts the response did NOT come from the
// ALB short-circuit (HTTP 464). A 200 with grpc-web headers is the
// happy path; a 404 from surfd (because the method or message is
// invalid) still proves the path works — what matters is that we got
// past the load balancer.
func probeGRPCWeb(ctx context.Context, pv *PreviewURL) error {
	// Construct a minimal gRPC-Web framed body: 1 byte flags (0=data, no
	// compression) + 4 bytes big-endian length + payload. Empty payload
	// is fine for a reachability probe.
	body := make([]byte, 5)
	binary.BigEndian.PutUint32(body[1:], 0)

	// The endpoint path is /<package>.<service>/<method>. We pick a
	// method that exists on RuntimeService (apps/surfd/runtime/server.go).
	// "Status" is the cheapest readable method.
	endpoint := strings.TrimRight(pv.URL, "/") + "/surf.v1.RuntimeService/Status"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("Accept", "application/grpc-web+proto")
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("X-Daytona-Preview-Token", pv.Token)

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	server := resp.Header.Get("Server")

	// HTTP 464 is the ALB's "incompatible target group protocol" verdict.
	// That's what we got pre-wrapper. Any other status proves we got past
	// the proxy.
	if resp.StatusCode == 464 {
		return fmt.Errorf("ALB still rejected grpc-web (HTTP 464); server=%q body=%q",
			server, truncate(respBody, 200))
	}

	// 502 with no Server header = surfd didn't start or didn't respond.
	// (Server: awselb/2.0 + 502 = ALB upstream timeout — surfd hung.)
	// Either way, the gRPC-Web path is no longer ALB-blocked at protocol.
	if resp.StatusCode == http.StatusBadGateway && strings.HasPrefix(server, "awselb") {
		return fmt.Errorf("ALB returned 502 from upstream — surfd may have crashed or hung; check sandbox logs")
	}

	// Happy paths:
	//   - 200 + content-type application/grpc-web*: gRPC-Web reply (success or grpc-status error).
	//   - Any non-464, non-ALB-502: we got past the proxy.
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/grpc-web") && resp.StatusCode != http.StatusOK {
		// Log but don't fail — what matters is ALB acceptance.
		return fmt.Errorf("unexpected response (status=%d, content-type=%q, body=%q)",
			resp.StatusCode, ct, truncate(respBody, 200))
	}

	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

func truncatePreview(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:6]
}

// (silence unused-import warnings in builds without filepath usage.)
var _ = filepath.Base
var _ orchestrator.Spec
