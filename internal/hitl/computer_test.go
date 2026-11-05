package hitl

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEvaluateComputer(t *testing.T) {
	spec := Spec{Gates: []Gate{
		{Name: "typing", On: KindComputer, Match: GateMatch{Action: []string{"type", "key"}}},
		{Name: "card", On: KindComputer, Match: GateMatch{TextContains: []string{"4242"}}, Mode: ModeDeny},
		{Name: "drag", On: KindComputer, Match: GateMatch{Action: []string{"left_click_drag"}}, Mode: ModeNotify},
	}}
	cases := []struct {
		call ComputerCall
		want Mode
	}{
		{ComputerCall{Action: "left_click"}, ""},
		{ComputerCall{Action: "type", Text: "hello"}, ModeAsk},
		{ComputerCall{Action: "key", Text: "ctrl+c"}, ModeAsk},
		{ComputerCall{Action: "type", Text: "card 4242 4242"}, ModeDeny},
		{ComputerCall{Action: "key", Text: "4242"}, ModeAsk},
		{ComputerCall{Action: "left_click_drag"}, ModeNotify},
	}
	for _, c := range cases {
		if got := spec.EvaluateComputer(c.call).Mode(); got != c.want {
			t.Errorf("%+v: mode = %q, want %q", c.call, got, c.want)
		}
	}
	ci := Spec{Gates: []Gate{{Name: "pw", On: KindComputer, Match: GateMatch{Action: []string{"type"}, TextContains: []string{"PassWord"}}}}}
	if ci.EvaluateComputer(ComputerCall{Action: "type", Text: "my password is"}).Mode() != ModeAsk {
		t.Error("text_contains must ignore case")
	}
}

func TestComputerGateValidation(t *testing.T) {
	ask := Requests{Ask: &AskRequests{}}
	cases := map[string]Gate{
		"needs action or text_contains":   {Name: "c", On: KindComputer},
		"must be one of screenshot":       {Name: "c", On: KindComputer, Match: GateMatch{Action: []string{"pay"}}},
		"only applies to the type action": {Name: "c", On: KindComputer, Match: GateMatch{Action: []string{"key"}, TextContains: []string{"x"}}},
		"matches only action and":         {Name: "c", On: KindComputer, Match: GateMatch{Program: []string{"git"}}},
		"only applies to on: computer":    {Name: "c", On: KindExec, Match: GateMatch{Program: []string{"git"}, TextContains: []string{"x"}}},
		"only an ask gate can have":       {Name: "c", On: KindComputer, Mode: ModeDeny, Match: GateMatch{Action: []string{"type"}}, Decide: &Decider{URL: "https://d.example", Secret: "S"}},
		"decide.url":                      {Name: "c", On: KindComputer, Match: GateMatch{Action: []string{"type"}}, Decide: &Decider{URL: "ftp://d", Secret: "S"}},
		"decide.secret":                   {Name: "c", On: KindExec, Match: GateMatch{Program: []string{"git"}}, Decide: &Decider{URL: "https://d.example"}},
		"has no action for a decider":     {Name: "s", On: KindStall, Match: GateMatch{Failures: 2}, Decide: &Decider{URL: "https://d.example", Secret: "S"}},
	}
	for want, g := range cases {
		err := Spec{Requests: ask, Gates: []Gate{g}}.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	ok := Spec{Requests: ask, Gates: []Gate{
		{Name: "c", On: KindComputer, Match: GateMatch{Action: []string{"type"}, TextContains: []string{"x"}}, Decide: &Decider{URL: "https://d.example", Secret: "S"}},
		{Name: "l", On: KindLifecycle, Match: GateMatch{Action: []string{"destroy"}}, Decide: &Decider{URL: "http://localhost:9/d", Secret: "S"}},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	if got := len(ok.Deciders()); got != 2 {
		t.Fatalf("deciders = %d, want 2", got)
	}
}

func TestComputerInputEdit(t *testing.T) {
	p, err := ParseComputerInput([]byte(`{"action":"type","text":"rm -rf /","future_field":7}`))
	if err != nil {
		t.Fatal(err)
	}
	in := Intervention{Kind: KindComputer, Computer: &p}
	if err := ValidateEditedText(in, "ls"); err != nil {
		t.Fatal(err)
	}
	edited := "ls"
	in.Decision = &Decision{Outcome: OutcomeApprove, EditedText: &edited}
	out, err := in.ComputerInput()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "ls" || got["future_field"] != float64(7) || got["action"] != "type" {
		t.Fatalf("edited input = %s", out)
	}
	click, _ := ParseComputerInput([]byte(`{"action":"left_click","coordinate":[1,2]}`))
	if err := ValidateEditedText(Intervention{Kind: KindComputer, Computer: &click}, "x"); err == nil {
		t.Fatal("editing the text of a click must fail")
	}
	if _, err := ParseComputerInput([]byte(`{"text":"x"}`)); err == nil {
		t.Fatal("an action without a name must fail")
	}
	if !(Intervention{Mode: ModeAsk, Computer: &p}).Runs() {
		t.Fatal("an ask-held computer action runs on approval")
	}
}

func TestParseDeciderResponse(t *testing.T) {
	for body, want := range map[string]DeciderVerdict{
		`{"decision":"allow"}`:                DeciderAllow,
		`{"decision":" DENY ","reason":"no"}`: DeciderDeny,
		`{"decision":"ask"}`:                  DeciderAsk,
	} {
		r, err := ParseDeciderResponse([]byte(body))
		if err != nil || r.Decision != want {
			t.Errorf("%s: %v, %v", body, r.Decision, err)
		}
	}
	for _, body := range []string{`{"decision":"maybe"}`, `not json`, `{}`} {
		if _, err := ParseDeciderResponse([]byte(body)); err == nil {
			t.Errorf("%s: want an error", body)
		}
	}
	if a := DeciderActor("checkout"); a.Source != ActorDecider || a.ID != "decider:checkout" {
		t.Fatalf("decider actor = %+v", a)
	}
}

func TestReviewRequests(t *testing.T) {
	diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n"
	p, err := ValidateReviewRequest(ReviewPayload{Content: diff})
	if err != nil || p.Format != ReviewDiff {
		t.Fatalf("diff = %+v, %v", p, err)
	}
	if p, _ := ValidateReviewRequest(ReviewPayload{Content: "plain"}); p.Format != ReviewText {
		t.Fatalf("text format = %q", p.Format)
	}
	if p, _ := ValidateReviewRequest(ReviewPayload{Path: "/work/fix.patch"}); p.Format != ReviewDiff {
		t.Fatalf("patch path format = %q", p.Format)
	}
	for _, bad := range []ReviewPayload{
		{},
		{Path: "/a", Content: "b"},
		{Path: "rel/path"},
		{Path: "/a/../etc"},
		{Content: "x", Format: "html"},
		{Content: strings.Repeat("x", MaxReviewBytes+1)},
	} {
		if _, err := ValidateReviewRequest(bad); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
	spec := Spec{Requests: Requests{Review: &ReviewRequests{}}}
	if err := spec.Validate(); err != nil || !spec.Allows(KindReview) {
		t.Fatalf("review-only spec: %v", err)
	}
	// comments reach the agent on both outcomes as the reason.
	in := Intervention{ID: "int_r", Kind: KindReview, State: StatePending, Review: &p}
	out, err := Decide(in, OutcomeReject, "", "line 3 leaks a token", Actor{ID: "alice"}, time.Now())
	if err != nil || out.Decision.Reason != "line 3 leaks a token" {
		t.Fatalf("reject review = %+v, %v", out.Decision, err)
	}
	if _, err := Decide(in, OutcomeApprove, "an answer", "", Actor{ID: "alice"}, time.Now()); err == nil {
		t.Fatal("approving a review with an answer must fail")
	}
}

func TestTakeoverViaDesktop(t *testing.T) {
	r := &TakeoverRequests{Via: []TakeoverVia{ViaDesktop}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if !r.Allows(ViaDesktop) || r.Allows(ViaTerminal) {
		t.Fatal("via desktop only allows desktop")
	}
}
