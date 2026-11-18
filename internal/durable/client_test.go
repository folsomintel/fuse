package durable

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestClientMarksRestateUnavailable: a call that cannot connect to ingress,
// refused or timed out, carries orchestrator.ErrRestateUnavailable, so the
// api answers 503.
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

	// a restate that never answers the dial (a dropped route): the request's
	// deadline ends the dial, and nothing reached restate.
	blackholed := NewClient(Config{Mode: ModeSelf, IngressURL: "http://192.0.2.1:9"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := blackholed.Destroy(ctx, "fuse-x", orchestrator.DestroyRequest{Reason: "api"}); !errors.Is(err, orchestrator.ErrRestateUnavailable) {
		t.Fatalf("destroy with restate unreachable: %v, want ErrRestateUnavailable", err)
	}
}

// TestClientLeavesAcceptedCallsUnmarked: a connection that drops after the
// request reached restate is not "unavailable": the invocation may run on.
func TestClientLeavesAcceptedCallsUnmarked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	c := NewClient(Config{Mode: ModeSelf, IngressURL: srv.URL})
	err := c.Destroy(context.Background(), "fuse-x", orchestrator.DestroyRequest{Reason: "api"})
	if err == nil || errors.Is(err, orchestrator.ErrRestateUnavailable) {
		t.Fatalf("destroy with the connection dropped after the request: %v, want a plain failure", err)
	}
}
