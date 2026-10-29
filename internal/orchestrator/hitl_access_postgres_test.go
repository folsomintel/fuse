package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/apikeys"
	"github.com/folsomintel/fuse/internal/hitl"
)

// TestPostgresHitlAccess covers the parts of #310 that only real postgres
// can: text[] scopes round trip, expiry and env revocation in the key
// store, and the audit chain under the advisory lock.
func TestPostgresHitlAccess(t *testing.T) {
	store := openTestPostgres(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	keys := apikeys.NewAPIKeyStore(store.db)

	// a full-access key keeps null scopes.
	full, fullRaw, err := keys.Create(ctx, apikeys.NewKey{Label: "agent", CreatedBy: "master"}, now)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := keys.Authenticate(ctx, fullRaw)
	if !ok || rec.Scopes != nil || rec.ID != full.ID || !rec.FirstUse {
		t.Fatalf("full key = %+v ok=%v", rec, ok)
	}
	if rec, _ = keys.Authenticate(ctx, fullRaw); rec.FirstUse {
		t.Fatal("second use reported as first")
	}

	env := "fuse-pg-" + now.Format("150405.000000")
	exp := now.Add(time.Hour)
	_, invRaw, err := keys.Create(ctx, apikeys.NewKey{
		Scopes: []string{"hitl:read", "hitl:decide"}, EnvID: env, ExpiresAt: &exp,
		ActorID: "alice", ActorName: "Alice", CreatedBy: "master",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, ok := keys.Authenticate(ctx, invRaw)
	if !ok || len(inv.Scopes) != 2 || inv.Scopes[1] != "hitl:decide" || inv.EnvID != env || inv.ActorID != "alice" {
		t.Fatalf("invite = %+v ok=%v", inv, ok)
	}

	past := now.Add(-time.Minute)
	_, oldRaw, err := keys.Create(ctx, apikeys.NewKey{Scopes: []string{"hitl:read"}, ExpiresAt: &past}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.Authenticate(ctx, oldRaw); ok {
		t.Fatal("expired key authenticated")
	}

	if err := keys.RebindEnv(ctx, env, env+"-m"); err != nil {
		t.Fatal(err)
	}
	if err := keys.RevokeEnv(ctx, env+"-m", now); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.Authenticate(ctx, invRaw); ok {
		t.Fatal("invite survived its env being destroyed")
	}

	// audit rows chain across appends, including concurrent ones.
	before, err := store.ListHitlAudit(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := store.AppendHitlAudit(ctx, hitl.AuditEntry{
				Event: hitl.AuditRaised, EnvID: env, At: time.Now(),
				Actor: hitl.Actor{ID: "alice", Source: hitl.ActorPinned, KeyID: inv.ID},
			})
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListHitlAudit(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(before)+8 {
		t.Fatalf("rows = %d, want %d", len(all), len(before)+8)
	}
	if err := hitl.VerifyChain(all); err != nil {
		t.Fatalf("chain: %v", err)
	}
	mine, _ := store.ListHitlAudit(ctx, AuditFilter{EnvIDs: []string{env}, ActorID: "alice", Limit: 3})
	if len(mine) != 3 {
		t.Fatalf("filtered rows = %d, want 3", len(mine))
	}

	// a hand edit breaks verification at that row.
	target := all[len(all)-3].Seq
	if _, err := store.db.ExecContext(ctx, `UPDATE orchestrator_hitl_audit SET actor_id = 'mallory' WHERE seq = $1`, target); err != nil {
		t.Fatal(err)
	}
	all, _ = store.ListHitlAudit(ctx, AuditFilter{})
	var ce *hitl.ChainError
	if err := hitl.VerifyChain(all); err == nil || !asChainError(err, &ce) || ce.Seq != target {
		t.Fatalf("tampered chain: %v, want a break at seq %d", err, target)
	}
	// restore it so a rerun against the same database starts intact.
	_, _ = store.db.ExecContext(ctx, `UPDATE orchestrator_hitl_audit SET actor_id = 'alice' WHERE seq = $1`, target)
}

func asChainError(err error, target **hitl.ChainError) bool {
	ce, ok := err.(*hitl.ChainError)
	*target = ce
	return ok
}

func TestPostgresHitlLineageAndFilters(t *testing.T) {
	store := openTestPostgres(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := "fuse-lineage-" + now.Format("150405.000000")
	if err := store.UpsertVM(ctx, VMRecord{
		ID: id, State: VMStateRunning, Spec: Spec{Ancestors: []string{"fuse-root", "fuse-mid"}},
		HitlKeysEncrypted: []byte{1, 2, 3}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	vms, err := store.ListVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vms {
		if v.ID == id {
			found = true
			if len(v.Spec.Ancestors) != 2 || v.Spec.Ancestors[1] != "fuse-mid" || len(v.HitlKeysEncrypted) != 3 {
				t.Fatalf("vm = %+v", v)
			}
		}
	}
	if !found {
		t.Fatal("vm not listed")
	}

	rec := InterventionRecord{
		ID: "int_" + id, EnvID: id, Kind: "ask", State: "pending", OnTimeout: "reject",
		TriggeredBy: []byte(`{"id":"bob","source":"asserted"}`),
		Deadline:    now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if err := store.UpsertIntervention(ctx, rec); err != nil {
		t.Fatal(err)
	}
	count := func(f InterventionFilter) int {
		recs, err := store.ListInterventions(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range recs {
			if r.ID == rec.ID {
				n++
			}
		}
		return n
	}
	if count(InterventionFilter{EnvIDs: []string{id}}) != 1 || count(InterventionFilter{EnvIDs: []string{}}) != 0 {
		t.Fatal("env id list filter")
	}
	if count(InterventionFilter{ActorID: "bob"}) != 1 || count(InterventionFilter{ActorID: "carol"}) != 0 {
		t.Fatal("actor filter")
	}
}
