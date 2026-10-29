package hitl

import (
	"strings"
	"testing"
	"time"
)

func TestSpecValidate(t *testing.T) {
	ask := Requests{Ask: &AskRequests{}}
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"minimal", Spec{Requests: ask}, ""},
		{"full", Spec{TimeoutSeconds: 600, OnTimeout: OnTimeoutAllow, Requests: ask}, ""},
		{"no requests", Spec{}, "declare at least one request kind"},
		{"negative timeout", Spec{TimeoutSeconds: -1, Requests: ask}, "must not be negative"},
		{"timeout too short", Spec{TimeoutSeconds: 5, Requests: ask}, "must be between"},
		{"timeout too long", Spec{TimeoutSeconds: 2 * 86400, Requests: ask}, "must be between"},
		{"bad on_timeout", Spec{OnTimeout: "maybe", Requests: ask}, "hitl.on_timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestSpecDefaults(t *testing.T) {
	var s Spec
	if s.Timeout() != DefaultTimeout {
		t.Fatalf("Timeout() = %s, want %s", s.Timeout(), DefaultTimeout)
	}
	if s.EffectiveOnTimeout() != OnTimeoutReject {
		t.Fatalf("EffectiveOnTimeout() = %q, want reject", s.EffectiveOnTimeout())
	}
	if s.Allows(KindAsk) {
		t.Fatal("an empty spec must not allow ask")
	}
	s.Requests.Ask = &AskRequests{}
	if !s.Allows(KindAsk) {
		t.Fatal("a declared ask must be allowed")
	}
	if s.Allows("secret") {
		t.Fatal("an unknown kind must never be allowed")
	}
}

func TestAskPayload(t *testing.T) {
	if err := (AskPayload{}).Validate(); err == nil {
		t.Fatal("empty question must be rejected")
	}
	if err := (AskPayload{Question: "q", Choices: []string{"a", "a"}}).Validate(); err == nil {
		t.Fatal("duplicate choices must be rejected")
	}
	p := AskPayload{Question: "deploy?", Choices: []string{"yes", "no"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	if err := p.ValidateAnswer("maybe"); err == nil {
		t.Fatal("an answer outside choices must be rejected")
	}
	if err := p.ValidateAnswer("yes"); err != nil {
		t.Fatalf("valid answer rejected: %v", err)
	}
	if err := (AskPayload{Question: "q"}).ValidateAnswer("  "); err == nil {
		t.Fatal("a blank answer must be rejected")
	}
}

func TestNewIDSortsByTime(t *testing.T) {
	t0 := time.Unix(1000, 0)
	a, b := NewID(t0), NewID(t0.Add(time.Nanosecond))
	if !(a < b) {
		t.Fatalf("ids do not sort by time: %s >= %s", a, b)
	}
	if !strings.HasPrefix(a, "int_") {
		t.Fatalf("id %q lacks the int_ prefix", a)
	}
}
