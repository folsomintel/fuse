package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestBuildsCreate_sendsBodyAndKey(t *testing.T) {
	var keys []string
	var got CreateBuildRequest
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/builds" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"bld_1","name":"app","state":"running","step":"queued","cache_hits":0,"layers":[{"index":0,"key":"k0","status":"miss"}],"created_at":"2026-11-15T00:00:00Z","updated_at":"2026-11-15T00:00:00Z"}`))
	})
	defer done()

	req := CreateBuildRequest{
		Name:  "app",
		Arch:  "amd64",
		Setup: BuildSetup{Prelude: "set -eu\n", Steps: []BuildStep{{Script: "npm ci", Key: "k0"}}},
	}
	ctx := context.Background()
	b, err := c.Builds.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "bld_1" || b.State != BuildRunning || len(b.Layers) != 1 || b.Layers[0].Status != "miss" {
		t.Fatalf("build %+v", b)
	}
	if got.Name != "app" || got.Arch != "amd64" || got.Setup.Steps[0].Key != "k0" {
		t.Fatalf("sent %+v", got)
	}
	if _, err := c.Builds.Create(ctx, req, WithIdempotencyKey("mine")); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[1] != "mine" {
		t.Fatalf("keys = %q, want an auto key then \"mine\"", keys)
	}
}

func TestBuildsWait_pollsUntilFinished(t *testing.T) {
	polls := 0
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/builds/bld_1" || r.URL.Query().Get("wait") == "" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
		}
		polls++
		if polls == 1 {
			_, _ = w.Write([]byte(`{"id":"bld_1","state":"running","step":"layer_0","layers":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"bld_1","state":"failed","step":"tail","error":"setup tail exited 1","output":"boom","layers":[{"index":0,"status":"hit","snapshot_id":"snap-0"}]}`))
	})
	defer done()

	b, err := c.Builds.Wait(context.Background(), "bld_1")
	if err != nil {
		t.Fatal(err)
	}
	if polls != 2 || b.State != BuildFailed || b.Output != "boom" || b.Layers[0].SnapshotID != "snap-0" {
		t.Fatalf("build %+v after %d polls", b, polls)
	}
}

func TestBuildsWait_boundedByContext(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"bld_1","state":"running","layers":[]}`))
	})
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Builds.Wait(ctx, "bld_1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

func TestBuildsGet_notFound(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"build not found"}}`))
	})
	defer done()
	if _, err := c.Builds.Get(context.Background(), "bld_x", GetBuildOptions{}); !IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestEventDecodesBuild(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"id":"e","event":"build","vm_id":"fuse-build-bld_1","state":"running","build":{"id":"bld_1","state":"running","step":"layer_1"}}`), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventKindBuild || ev.Build == nil || ev.Build.Step != "layer_1" {
		t.Fatalf("event %+v", ev)
	}
}
