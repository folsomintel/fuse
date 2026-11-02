package hitl

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestResolveActor(t *testing.T) {
	alice := &Actor{ID: "alice", Name: "Alice"}
	bob := &Actor{ID: "bob"}
	cases := []struct {
		name    string
		cred    Credential
		named   *Actor
		want    Actor
		wantErr error
	}{
		{"master default", Credential{Master: true}, nil, Actor{ID: "master", Source: ActorKey}, nil},
		{"master asserts", Credential{Master: true}, bob, Actor{ID: "bob", Source: ActorAsserted}, nil},
		{"pinned", Credential{KeyID: "ak_1", Pinned: alice}, nil, Actor{ID: "alice", Name: "Alice", Source: ActorPinned, KeyID: "ak_1"}, nil},
		{"pinned same header", Credential{KeyID: "ak_1", Pinned: alice}, &Actor{ID: "alice"}, Actor{ID: "alice", Name: "Alice", Source: ActorPinned, KeyID: "ak_1"}, nil},
		{"pinned other header", Credential{KeyID: "ak_1", Pinned: alice}, bob, Actor{}, ErrActorMismatch},
		{"act-as asserts", Credential{KeyID: "ak_2", ActAs: true}, bob, Actor{ID: "bob", Source: ActorAsserted, KeyID: "ak_2"}, nil},
		{"act-as no header", Credential{KeyID: "ak_2", Label: "app", ActAs: true}, nil, Actor{ID: "key:ak_2", Name: "app", Source: ActorKey, KeyID: "ak_2"}, nil},
		{"plain key", Credential{KeyID: "ak_3", Label: "agent"}, nil, Actor{ID: "key:ak_3", Name: "agent", Source: ActorKey, KeyID: "ak_3"}, nil},
		{"plain key header", Credential{KeyID: "ak_3"}, bob, Actor{}, ErrActorNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveActor(tc.cred, tc.named)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("actor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseActorHeaders(t *testing.T) {
	if a, err := ParseActorHeaders("", ""); a != nil || err != nil {
		t.Fatalf("no headers: got %v, %v", a, err)
	}
	for _, tc := range []struct{ id, name string }{
		{"", "Alice"},
		{strings.Repeat("a", MaxActorIDLen+1), ""},
		{"alice", strings.Repeat("n", MaxActorNameLen+1)},
		{"al\nice", ""},
	} {
		if _, err := ParseActorHeaders(tc.id, tc.name); !errors.Is(err, ErrInvalidActor) {
			t.Errorf("ParseActorHeaders(%q, %q) = %v, want ErrInvalidActor", tc.id, tc.name, err)
		}
	}
}

func TestCheckDecider(t *testing.T) {
	agent := Actor{ID: "key:ak_agent", Source: ActorKey, KeyID: "ak_agent"}
	master := Actor{ID: "master", Source: ActorKey}
	alice := Actor{ID: "alice", Source: ActorPinned, KeyID: "ak_alice"}
	cases := []struct {
		name         string
		triggered    Actor
		decider      Actor
		requireActor bool
		want         error
	}{
		{"same key", agent, agent, false, ErrSelfApproval},
		{"same key other actor", Actor{ID: "run-1", Source: ActorAsserted, KeyID: "ak_x"}, Actor{ID: "carol", Source: ActorAsserted, KeyID: "ak_x"}, false, ErrSelfApproval},
		{"same actor other key", Actor{ID: "bob", Source: ActorAsserted, KeyID: "ak_1"}, Actor{ID: "bob", Source: ActorAsserted, KeyID: "ak_2"}, false, ErrSelfApproval},
		{"invite decides agent ask", agent, alice, false, nil},
		{"master raises and decides", master, master, false, nil},
		{"master asserting the raiser", Actor{ID: "bob", Source: ActorAsserted}, Actor{ID: "bob", Source: ActorAsserted}, false, ErrSelfApproval},
		{"require actor, unnamed", agent, master, true, ErrActorRequired},
		{"require actor, pinned", agent, alice, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckDecider(tc.triggered, tc.decider, tc.requireActor); !errors.Is(err, tc.want) {
				t.Fatalf("CheckDecider = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGrants(t *testing.T) {
	if !Grants(nil, ScopeRead) || !Grants(nil, ScopeTakeover) {
		t.Error("full-access key should hold read and takeover")
	}
	for _, s := range []Scope{ScopeDecide, ScopeSecret, ScopeActAs} {
		if Grants(nil, s) {
			t.Errorf("full-access key must not hold %s", s)
		}
	}
	if !Grants([]Scope{ScopeDecide}, ScopeRead) {
		t.Error("decide should imply read")
	}
	if Grants([]Scope{ScopeRead}, ScopeDecide) {
		t.Error("read must not imply decide")
	}
	if Grants([]Scope{}, ScopeRead) {
		t.Error("an empty scope list holds nothing")
	}
}

func TestParseScopes(t *testing.T) {
	got, err := ParseScopes([]string{"hitl:decide", "hitl:read", "hitl:decide"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != ScopeRead || got[1] != ScopeDecide {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseScopes([]string{"admin"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

func chain(n int) []AuditEntry {
	out := make([]AuditEntry, 0, n)
	prev := GenesisHash
	at := time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
	for i := 1; i <= n; i++ {
		e := AuditEntry{
			Seq: int64(i), Event: AuditRaised, InterventionID: "int_x", EnvID: "fuse-a",
			Actor: Actor{ID: "alice", Source: ActorPinned, KeyID: "ak_1"}, RemoteAddr: "10.0.0.1:5000",
			At: AuditTime(at.Add(time.Duration(i) * time.Second)), PrevHash: prev,
		}
		e.Hash = e.ComputeHash()
		prev = e.Hash
		out = append(out, e)
	}
	return out
}

func TestVerifyChain(t *testing.T) {
	if err := VerifyChain(chain(4)); err != nil {
		t.Fatalf("intact chain: %v", err)
	}

	edited := chain(4)
	edited[1].Actor.ID = "mallory"
	var ce *ChainError
	if err := VerifyChain(edited); !errors.As(err, &ce) || ce.Seq != 2 {
		t.Fatalf("edited row: got %v, want a break at seq 2", err)
	}

	removed := chain(4)
	removed = append(removed[:2], removed[3:]...)
	if err := VerifyChain(removed); !errors.As(err, &ce) || ce.Seq != 4 {
		t.Fatalf("removed row: got %v, want a break at seq 4", err)
	}
}

func TestWebhookSignature(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"event":"raised"}`)
	now := time.Unix(1_800_000_000, 0)
	h := Sign(secret, now, body)
	if err := VerifySignature(secret, h, body, now, 5*time.Minute); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	if err := VerifySignature([]byte("other"), h, body, now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatal("wrong secret verified")
	}
	if err := VerifySignature(secret, h, []byte(`{"event":"decided"}`), now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatal("tampered body verified")
	}
	if err := VerifySignature(secret, h, body, now.Add(time.Hour), 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatal("stale signature verified")
	}
}

func TestSpecValidateNotify(t *testing.T) {
	base := Spec{Requests: Requests{Ask: &AskRequests{}}}
	ok := base
	ok.Notify = []Webhook{{URL: "https://hooks.example.com/fuse", Secret: "HOOK"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid notify: %v", err)
	}
	for _, w := range []Webhook{
		{URL: "ftp://x", Secret: "HOOK"},
		{URL: "https://user:pw@x", Secret: "HOOK"},
		{URL: "https://x"},
	} {
		s := base
		s.Notify = []Webhook{w}
		if err := s.Validate(); err == nil {
			t.Errorf("notify %+v accepted", w)
		}
	}
}
