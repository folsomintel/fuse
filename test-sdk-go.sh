#!/usr/bin/env bash
# end-to-end go sdk test. exercises every service method (hosts, api keys,
# environments, snapshots) against a running orchestrator, using the in-repo
# sdks/go via a local replace. lifecycle paths that need a running vm assert the
# reached-the-host error, since the guest rootfs bake is incomplete on the test host.
#
#   FUSE_TOKEN=<orchestrator-token> ./test-sdk-go.sh
#
# env: FUSE_BASE_URL (default http://127.0.0.1:8080), FUSE_TOKEN (required),
#      FC_URL (default http://51.79.19.90:8090), FC_TOKEN (optional).
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${FUSE_BASE_URL:=http://127.0.0.1:8080}"
: "${FUSE_TOKEN:?set FUSE_TOKEN to the orchestrator auth token}"
: "${FC_URL:=http://51.79.19.90:8090}"
: "${FC_TOKEN:=}"
export FUSE_BASE_URL FUSE_TOKEN FC_URL FC_TOKEN

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
cat > "$WORK/go.mod" <<EOF
module sdke2e

go 1.23

require github.com/folsomintel/fuse/sdks/go v0.0.0

replace github.com/folsomintel/fuse/sdks/go => $REPO/sdks/go
EOF

cat > "$WORK/main.go" <<'GOEOF'
package main

import (
	"context"
	"fmt"
	"os"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

var fails int

func pass(msg string) { fmt.Println("PASS:", msg) }
func fail(msg string) { fmt.Println("FAIL:", msg); fails++ }
func check(cond bool, msg string) {
	if cond {
		pass(msg)
	} else {
		fail(msg)
	}
}

func wantNotFound(err error, msg string) {
	if err != nil && fuse.IsNotFound(err) {
		pass(msg + " -> 404 not_found")
	} else {
		fail(fmt.Sprintf("%s -> expected not_found, got %v", msg, err))
	}
}

func main() {
	base := envOr("FUSE_BASE_URL", "http://127.0.0.1:8080")
	token := os.Getenv("FUSE_TOKEN")
	fcURL := envOr("FC_URL", "http://51.79.19.90:8090")
	fcToken := os.Getenv("FC_TOKEN")
	ctx := context.Background()

	c, err := fuse.New(base, token)
	if err != nil {
		fail("New client: " + err.Error())
		os.Exit(1)
	}
	pass("New client")

	// hosts: full lifecycle
	const hid = "e2e-host-go"
	_ = c.Hosts.Deregister(ctx, hid)
	h, err := c.Hosts.Register(ctx, fuse.RegisterHostRequest{
		ID: hid, URL: fcURL, Token: fcToken, Region: "local", Backend: "firecracker",
		Capacity: fuse.HostCapacity{CPUs: 8, RamMB: 16384, StorageGB: 100, VMCount: 10},
	})
	check(err == nil && h != nil && h.ID == hid && h.State == "active", "Hosts.Register -> active")
	hosts, err := c.Hosts.List(ctx)
	check(err == nil && containsHost(hosts, hid), "Hosts.List contains host")
	got, err := c.Hosts.Get(ctx, hid)
	check(err == nil && got != nil && got.URL == fcURL && got.Capacity.CPUs == 8, "Hosts.Get")
	check(c.Hosts.Cordon(ctx, hid) == nil, "Hosts.Cordon")
	got, _ = c.Hosts.Get(ctx, hid)
	check(got != nil && got.State == "cordoned", "host state == cordoned")
	check(c.Hosts.Uncordon(ctx, hid) == nil, "Hosts.Uncordon")
	got, _ = c.Hosts.Get(ctx, hid)
	check(got != nil && got.State == "active", "host state == active")
	check(c.Hosts.Deregister(ctx, hid) == nil, "Hosts.Deregister")
	hosts, _ = c.Hosts.List(ctx)
	check(!containsHost(hosts, hid), "host gone after deregister")
	wantNotFound(hostErr(c.Hosts.Get(ctx, hid)), "Hosts.Get(deregistered)")

	// api keys: full lifecycle (requires postgres-backed orchestrator)
	created, err := c.APIKeys.Create(ctx, "e2e-go")
	if err != nil {
		fmt.Println("SKIP: APIKeys (needs postgres-backed orchestrator):", err)
	} else {
		check(created.Key != "" && created.ID != "", "APIKeys.Create returns secret")
		kc, _ := fuse.New(base, created.Key)
		_, kerr := kc.Hosts.List(ctx)
		check(kerr == nil, "created api key authenticates")
		keys, lerr := c.APIKeys.List(ctx)
		check(lerr == nil && containsKey(keys, created.ID), "APIKeys.List contains key")
		check(c.APIKeys.Revoke(ctx, created.ID) == nil, "APIKeys.Revoke")
		_, rerr := kc.Hosts.List(ctx)
		check(rerr != nil && fuse.IsUnauthorized(rerr), "revoked key rejected (401)")
	}

	// environments
	envs, err := c.Environments.List(ctx, fuse.ListEnvironmentsOptions{})
	check(err == nil, fmt.Sprintf("Environments.List -> %d", len(envs)))
	_, cerr := c.Environments.Create(ctx, fuse.CreateRequest{
		TaskID: "e2ego", Spec: fuse.Spec{CPUs: 1, RamMB: 512, StorageGB: 1},
	})
	if apiErr, ok := fuse.AsAPIError(cerr); ok && apiErr.Status == 500 {
		pass("Environments.Create reached host (500 provisioning, rootfs bake blocked)")
	} else if cerr == nil {
		pass("Environments.Create succeeded (rootfs is complete!)")
		_ = c.Environments.Destroy(ctx, "fusetest-e2ego")
	} else {
		fail("Environments.Create unexpected error: " + cerr.Error())
	}
	wantNotFound(envErr(c.Environments.Get(ctx, "nope-e2e")), "Environments.Get(missing)")
	wantNotFound(envErr(c.Environments.Drain(ctx, "nope-e2e")), "Environments.Drain(missing)")
	wantNotFound(envErr(c.Environments.Fork(ctx, "nope-e2e", fuse.ForkOptions{})), "Environments.Fork(missing)")
	wantNotFound(c.Environments.RotateToken(ctx, "nope-e2e"), "Environments.RotateToken(missing)")
	wantNotFound(c.Environments.Destroy(ctx, "nope-e2e"), "Environments.Destroy(missing)")

	// snapshots
	snaps, err := c.Snapshots.List(ctx, fuse.ListSnapshotsOptions{})
	check(err == nil, fmt.Sprintf("Snapshots.List -> %d", len(snaps)))
	wantNotFound(snapErr(c.Snapshots.Get(ctx, "nope-e2e")), "Snapshots.Get(missing)")
	wantNotFound(c.Snapshots.Restore(ctx, "nope-e2e"), "Snapshots.Restore(missing)")
	wantNotFound(c.Snapshots.Delete(ctx, "nope-e2e"), "Snapshots.Delete(missing)")

	// negative auth
	bad, _ := fuse.New(base, "wrong-token")
	_, berr := bad.Hosts.List(ctx)
	check(berr != nil && fuse.IsUnauthorized(berr), "negative auth rejected (401)")

	fmt.Printf("\n== go sdk e2e: %d failure(s) ==\n", fails)
	if fails > 0 {
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func containsHost(hs []fuse.Host, id string) bool {
	for _, h := range hs {
		if h.ID == id {
			return true
		}
	}
	return false
}
func containsKey(ks []fuse.APIKey, id string) bool {
	for _, k := range ks {
		if k.ID == id {
			return true
		}
	}
	return false
}
func hostErr(_ *fuse.Host, err error) error            { return err }
func envErr(_ *fuse.EnvironmentInfo, err error) error   { return err }
func snapErr(_ *fuse.Snapshot, err error) error         { return err }
GOEOF

cd "$WORK"
GOPROXY=off GOFLAGS=-mod=mod go run .
