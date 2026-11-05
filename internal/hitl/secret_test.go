package hitl

import (
	"strings"
	"testing"
	"time"
)

func TestSpecValidatesSecretTakeoverAndSignals(t *testing.T) {
	ok := Spec{Requests: Requests{
		Secret: &SecretRequests{Targets: []SecretTarget{
			{Env: "GH_TOKEN", Label: "GitHub token"},
			{File: "/root/.netrc", Label: "netrc"},
		}},
		Takeover: &TakeoverRequests{Via: []TakeoverVia{ViaTerminal}},
	}, Gates: []Gate{
		{Name: "flailing", On: KindStall, Match: GateMatch{Failures: 3}},
		{Name: "quiet", On: KindStall, Mode: ModeNotify, Match: GateMatch{QuietSeconds: 900}},
		{Name: "extend", On: KindExpiry, Match: GateMatch{BeforeSeconds: 300}},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}

	cases := map[string]Spec{
		"set one of env or file":  {Requests: Requests{Secret: &SecretRequests{Targets: []SecretTarget{{Label: "x"}}}}},
		"label is required":       {Requests: Requests{Secret: &SecretRequests{Targets: []SecretTarget{{Env: "A"}}}}},
		"reserves":                {Requests: Requests{Secret: &SecretRequests{Targets: []SecretTarget{{File: "/fuse/x", Label: "x"}}}}},
		"must not contain ..":     {Requests: Requests{Secret: &SecretRequests{Targets: []SecretTarget{{File: "/a/../etc/shadow", Label: "x"}}}}},
		"not yet supported":       {Requests: Requests{Takeover: &TakeoverRequests{Via: []TakeoverVia{"port"}}}},
		"needs failures or quiet": {Requests: Requests{Ask: &AskRequests{}}, Gates: []Gate{{Name: "s", On: KindStall}}},
		"needs before":            {Requests: Requests{Ask: &AskRequests{}}, Gates: []Gate{{Name: "e", On: KindExpiry}}},
		"no action to deny":       {Requests: Requests{Ask: &AskRequests{}}, Gates: []Gate{{Name: "s", On: KindStall, Mode: ModeDeny, Match: GateMatch{Failures: 1}}}},
		"cannot grant":            {Requests: Requests{Ask: &AskRequests{}}, Gates: []Gate{{Name: "s", On: KindStall, Grant: "env", Match: GateMatch{Failures: 1}}}},
		"must be between 1m0s":    {Requests: Requests{Ask: &AskRequests{}}, Gates: []Gate{{Name: "e", On: KindExpiry, Match: GateMatch{BeforeSeconds: 5}}}},
		"matches only failures":   {Requests: Requests{Ask: &AskRequests{}}, Gates: []Gate{{Name: "s", On: KindStall, Match: GateMatch{Failures: 1, Program: []string{"git"}}}}},
	}
	for want, s := range cases {
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestTakeoverAndSecretTransitions(t *testing.T) {
	now := time.Now()
	in := Intervention{ID: "int_1", Kind: KindTakeover, State: StatePending, Takeover: &TakeoverPayload{Via: ViaTerminal}}
	if in.Locks() {
		t.Fatal("an unclaimed takeover must not lock")
	}
	if _, err := ReleaseTakeover(in, "", Actor{ID: "a"}, now); err == nil {
		t.Fatal("released a takeover nobody held")
	}
	held, err := StartTakeover(in, Actor{ID: "alice"}, now)
	if err != nil || !held.Locks() {
		t.Fatalf("start: %+v, %v", held, err)
	}
	if _, err := StartTakeover(held, Actor{ID: "bob"}, now); err == nil {
		t.Fatal("second holder accepted")
	}
	if _, err := Decide(held, OutcomeApprove, "", "", Actor{ID: "bob"}, now); err == nil {
		t.Fatal("plain approve accepted for a takeover")
	}
	out, err := ReleaseTakeover(held, "", Actor{ID: "alice"}, now)
	if err != nil || out.State != StateApproved || out.Decision.Answer != "released by alice" || out.Locks() {
		t.Fatalf("release: %+v, %v", out, err)
	}

	sec := Intervention{ID: "int_2", Kind: KindSecret, State: StatePending}
	if _, err := Decide(sec, OutcomeApprove, "the-value", "", Actor{ID: "a"}, now); err == nil {
		t.Fatal("plain approve accepted for a secret")
	}
	done, err := AnswerSecret(sec, Actor{ID: "a"}, now)
	if err != nil || done.Decision.Answer != SecretAnswer {
		t.Fatalf("answer: %+v, %v", done, err)
	}
}
