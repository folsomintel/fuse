package fuse

import "testing"

// under `go test` the module is the main module with version "(devel)", so
// sdkVersion falls back to "dev". this pins the fallback contract.
func TestSDKVersionFallback(t *testing.T) {
	if got := sdkVersion(); got != "dev" {
		t.Fatalf("sdkVersion() = %q, want %q in test context", got, "dev")
	}
}
