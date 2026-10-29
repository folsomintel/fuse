package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// fakeEnvKeys records revoke and rebind calls.
type fakeEnvKeys struct {
	mu      sync.Mutex
	revoked []string
	rebound [][2]string
}

func (k *fakeEnvKeys) RevokeEnv(_ context.Context, envID string, _ time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.revoked = append(k.revoked, envID)
	return nil
}

func (k *fakeEnvKeys) RebindEnv(_ context.Context, fromID, toID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rebound = append(k.rebound, [2]string{fromID, toID})
	return nil
}

// receiver is a webhook endpoint that keeps every delivery.
type receiver struct {
	mu   sync.Mutex
	got  []delivery
	srv  *httptest.Server
	fail int // answer 500 to this many deliveries first
}

type delivery struct {
	sig  string
	body []byte
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if rc.fail > 0 {
			rc.fail--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		rc.got = append(rc.got, delivery{sig: r.Header.Get(hitl.HeaderSignature), body: body})
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

// wait returns once n deliveries arrived.
func (rc *receiver) wait(t *testing.T, n int) []delivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rc.mu.Lock()
		if len(rc.got) >= n {
			out := append([]delivery(nil), rc.got...)
			rc.mu.Unlock()
			return out
		}
		rc.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d webhook deliveries, want %d", len(rc.got), n)
	return nil
}

func notifyFleet(t *testing.T, url string) (*FleetManager, string) {
	t.Helper()
	spec := askSpec()
	spec.Notify = []hitl.Webhook{{URL: url, Secret: "HOOK"}}
	fm, vmID := hitlFleet(t, FleetConfig{}, spec)
	fm.mu.Lock()
	fm.vms[vmID].hitlKeys = map[string]string{"HOOK": "whsec"}
	fm.mu.Unlock()
	fm.webhooks.backoff = []time.Duration{0, 0, 0}
	fm.SetPublicURL("https://fuse.example.com")
	return fm, vmID
}

func TestHitlWebhookIsSignedAndContentFree(t *testing.T) {
	rc := newReceiver(t)
	rc.fail = 1 // the first attempt fails and is retried
	fm, vmID := notifyFleet(t, rc.srv.URL)

	in, err := ask(fm, vmID)
	if err != nil {
		t.Fatal(err)
	}
	d := rc.wait(t, 1)[0]
	if err := hitl.VerifySignature([]byte("whsec"), d.sig, d.body, time.Now(), time.Minute); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if strings.Contains(string(d.body), "deploy to prod") {
		t.Fatalf("webhook body leaked the payload: %s", d.body)
	}
	var n map[string]any
	if err := json.Unmarshal(d.body, &n); err != nil {
		t.Fatal(err)
	}
	for k := range n {
		switch k {
		case "event", "intervention_id", "env_id", "gate", "on", "deadline", "url":
		default:
			t.Errorf("unexpected webhook field %q", k)
		}
	}
	if n["event"] != "raised" || n["intervention_id"] != in.ID || n["on"] != "ask" ||
		n["url"] != "https://fuse.example.com/v1/interventions/"+in.ID {
		t.Fatalf("notification = %v", n)
	}
}

func TestDestroyCancelsNotifiesAndRevokesKeys(t *testing.T) {
	rc := newReceiver(t)
	fm, vmID := notifyFleet(t, rc.srv.URL)
	keys := &fakeEnvKeys{}
	fm.SetEnvKeyStore(keys)

	if _, err := ask(fm, vmID); err != nil {
		t.Fatal(err)
	}
	if err := fm.DestroyVM(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	// deliveries are concurrent, so look for the cancel rather than
	// assume it arrived second.
	cancelled := false
	for _, d := range rc.wait(t, 2) {
		var n hitl.Notification
		_ = json.Unmarshal(d.body, &n)
		cancelled = cancelled || n.Event == hitl.AuditCancelled
	}
	if !cancelled {
		t.Fatal("no cancelled delivery after destroy")
	}
	if len(keys.revoked) != 1 || keys.revoked[0] != vmID {
		t.Fatalf("revoked = %v, want [%s]", keys.revoked, vmID)
	}
}

func TestDecisionsAreAudited(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	agent := hitl.Actor{ID: "key:ak_agent", Source: hitl.ActorKey, KeyID: "ak_agent"}
	in, err := fm.RequestHuman(context.Background(), vmID, HumanRequest{
		Kind: hitl.KindAsk, Ask: hitl.AskPayload{Question: "q"}, By: agent, RemoteAddr: "10.0.0.9:1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "a", By: agent}); err == nil {
		t.Fatal("the raising key decided its own intervention")
	}
	alice := hitl.Actor{ID: "alice", Source: hitl.ActorPinned, KeyID: "ak_alice"}
	if _, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "a", By: alice, RemoteAddr: "10.0.0.7:4321"}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := fm.ListHitlAudit(context.Background(), HitlAuditListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Event != hitl.AuditRaised || rows[1].Event != hitl.AuditDecided {
		t.Fatalf("audit rows = %+v", rows)
	}
	if rows[0].RemoteAddr != "10.0.0.9:1234" || rows[1].Actor != alice {
		t.Fatalf("audit rows = %+v", rows)
	}
	if err := hitl.VerifyChain(rows); err != nil {
		t.Fatal(err)
	}
}

func TestEnvCoveredFollowsForksOnly(t *testing.T) {
	fm, parent := hitlFleet(t, FleetConfig{}, askSpec())
	fm.mu.Lock()
	fm.vms["fuse-fork-1"] = &vm{id: "fuse-fork-1", spec: Spec{Ancestors: []string{parent}}}
	fm.vms["fuse-fork-2"] = &vm{id: "fuse-fork-2", spec: Spec{Ancestors: []string{parent, "fuse-fork-1"}}}
	fm.vms["fuse-other"] = &vm{id: "fuse-other"}
	fm.mu.Unlock()

	if !fm.EnvCovered(parent, "fuse-fork-2") || !fm.EnvCovered("fuse-fork-1", "fuse-fork-2") {
		t.Error("an invite should cover its env's forks")
	}
	if fm.EnvCovered("fuse-fork-1", parent) || fm.EnvCovered(parent, "fuse-other") {
		t.Error("an invite must not cover parents or siblings")
	}
	got := fm.EnvsCoveredBy(parent)
	if len(got) != 3 || got[0] != parent {
		t.Fatalf("EnvsCoveredBy = %v", got)
	}
}

func TestHitlKeysForNeedsTheNamedSecret(t *testing.T) {
	fm, _ := hitlFleet(t, FleetConfig{}, askSpec())
	spec := askSpec()
	spec.Notify = []hitl.Webhook{{URL: "https://x", Secret: "HOOK"}}
	if _, _, err := fm.hitlKeysFor(spec, map[string]string{}); err == nil {
		t.Fatal("missing secret accepted")
	}
	fm.tokenEncryptionKey = make([]byte, 32)
	keys, sealed, err := fm.hitlKeysFor(spec, map[string]string{"HOOK": "v"})
	if err != nil || keys["HOOK"] != "v" || len(sealed) == 0 {
		t.Fatalf("keys=%v sealed=%d err=%v", keys, len(sealed), err)
	}
	if strings.Contains(string(sealed), `"v"`) {
		t.Fatal("sealed blob holds the secret in the clear")
	}
	if got := fm.openHitlKeys("vm", sealed); got["HOOK"] != "v" {
		t.Fatalf("openHitlKeys = %v", got)
	}
}
