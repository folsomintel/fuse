// Package e2e holds end-to-end "deploy everything" tests for Fuse.
//
// deploy_test.go assembles the full orchestrator stack the way
// server/main.go does — FleetManager + the Firecracker provider + the chi
// REST router + the /health and /ready probes — and drives a complete VM
// lifecycle (provision → snapshot → restore → drain → destroy) over real
// HTTP against an httptest server.
//
// By default it runs against the in-memory Firecracker stub provider, so it is
// hermetic and needs no host, network, or KVM. To run the same lifecycle
// against a real Firecracker host, point it at one via FUSE_E2E_FIRECRACKER_URL
// / FUSE_E2E_FIRECRACKER_TOKEN (see deploy_test.go); when unset the test uses
// the stub.
package e2e
