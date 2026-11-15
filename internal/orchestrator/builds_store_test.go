package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"
)

// a build record survives the round trip through both stores, and a missing
// one reads as ErrBuildNotFound.
func TestBuildStoreRoundTrip(t *testing.T) {
	stores := map[string]func(*testing.T) BuildStore{
		"memory":   func(*testing.T) BuildStore { return NewMemoryStateStore() },
		"postgres": func(t *testing.T) BuildStore { return openTestPostgres(t) },
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			bs := open(t)
			ctx := context.Background()
			id := "bld_store_" + name
			t.Cleanup(func() { _ = bs.DeleteBuild(ctx, id) })

			now := time.Now().UTC().Truncate(time.Millisecond)
			rec := newBuildRecord(testBuildInput(id, "k0", "k1", ""), now)
			rec.Durable = true
			rec.Layers[0].SnapshotID = "snap-0"
			rec.FinishedAt = &now
			if err := bs.UpsertBuild(ctx, rec); err != nil {
				t.Fatal(err)
			}
			got, err := bs.GetBuild(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != id || !got.Durable || got.Layers[0].SnapshotID != "snap-0" ||
				got.Layers[2].Status != LayerSkip || got.FinishedAt == nil || !got.CreatedAt.Equal(now) {
				t.Fatalf("round trip %+v", got)
			}

			rec.State = BuildSucceeded
			if err := bs.UpsertBuild(ctx, rec); err != nil {
				t.Fatal(err)
			}
			if got, _ := bs.GetBuild(ctx, id); got.State != BuildSucceeded {
				t.Fatalf("update not kept: %s", got.State)
			}

			if err := bs.DeleteBuild(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, err := bs.GetBuild(ctx, id); !errors.Is(err, ErrBuildNotFound) {
				t.Fatalf("deleted build: err = %v", err)
			}
		})
	}
}
