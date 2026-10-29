package main

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

func TestParseScopesShorthand(t *testing.T) {
	got := parseScopes("decide, hitl:read,,takeover")
	want := []string{"hitl:decide", "hitl:read", "hitl:takeover"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseScopes = %v, want %v", got, want)
	}
	if parseScopes("") != nil {
		t.Fatal("empty --scopes must stay nil (full access)")
	}
}

func TestRenderActor(t *testing.T) {
	cases := map[string]fuse.Actor{
		"master":                 {ID: "master", Source: "key"},
		"alice (pinned)":         {ID: "alice", Source: "pinned", KeyID: "ak_1"},
		"bob (asserted by ak_2)": {ID: "bob", Source: "asserted", KeyID: "ak_2"},
		"system (system)":        {ID: "system", Source: "system"},
	}
	for want, a := range cases {
		if got := renderActor(a); got != want {
			t.Errorf("renderActor(%+v) = %q, want %q", a, got, want)
		}
	}
}

// auditRows builds a valid chain in the sdk's wire shape.
func auditRows(n int) []fuse.AuditEntry {
	prev := hitl.GenesisHash
	out := make([]fuse.AuditEntry, 0, n)
	for i := 1; i <= n; i++ {
		e := hitl.AuditEntry{
			Seq: int64(i), Event: hitl.AuditDecided, EnvID: "fuse-a", InterventionID: "int_1",
			Actor: hitl.Actor{ID: "alice", Source: hitl.ActorPinned, KeyID: "ak_1"},
			At:    hitl.AuditTime(time.Unix(1_800_000_000+int64(i), 0)), PrevHash: prev,
		}
		e.Hash = e.ComputeHash()
		prev = e.Hash
		out = append(out, fuse.AuditEntry{
			Seq: e.Seq, Event: string(e.Event), EnvID: e.EnvID, InterventionID: e.InterventionID,
			Actor: fuse.Actor{ID: e.Actor.ID, Source: string(e.Actor.Source), KeyID: e.Actor.KeyID},
			At:    e.At, PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash),
		})
	}
	return out
}

func TestVerifyAudit(t *testing.T) {
	if err := verifyAudit(auditRows(3)); err != nil {
		t.Fatalf("intact chain: %v", err)
	}
	rows := auditRows(3)
	rows[1].Actor.ID = "mallory"
	err := verifyAudit(rows)
	if err == nil || !strings.Contains(err.Error(), "seq 2") {
		t.Fatalf("tampered chain: %v, want a break at seq 2", err)
	}
}
