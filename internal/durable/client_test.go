package durable

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestClientMarksRestateUnavailable: a call ingress never answers carries
// orchestrator.ErrRestateUnavailable, so the api answers 503; one the
// caller's own context ended does not.
func TestClientMarksRestateUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c := NewClient(Config{Mode: ModeSelf, IngressURL: "http://" + addr})

	err = c.Destroy(context.Background(), "fuse-x", orchestrator.DestroyRequest{Reason: "api"})
	if !errors.Is(err, orchestrator.ErrRestateUnavailable) {
		t.Fatalf("destroy with restate down: %v, want ErrRestateUnavailable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Destroy(ctx, "fuse-x", orchestrator.DestroyRequest{Reason: "api"}); errors.Is(err, orchestrator.ErrRestateUnavailable) {
		t.Fatalf("cancelled destroy: %v, want the cancellation, not ErrRestateUnavailable", err)
	}
}
