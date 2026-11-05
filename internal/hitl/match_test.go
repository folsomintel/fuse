package hitl

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStripWrappers(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"git push --force", "git push --force"},
		{"env GIT_TRACE=1 git push --force", "git push --force"},
		{"/usr/bin/env -i A=1 B=2 git push", "git push"},
		{"env -u HOME git push", "git push"},
		{"env -S git_push_--force", "git_push_--force"},
		{"sudo git push --force", "git push --force"},
		{"sudo -u root -E git push", "git push"},
		{"sudo -Eu root git push", "git push"},
		{"sudo -- git push", "git push"},
		{"nice -n 10 git push", "git push"},
		{"nice -10 git push", "git push"},
		{"timeout 30 git push", "git push"},
		{"timeout -s KILL -k 5 30 git push", "git push"},
		{"timeout --signal=KILL 30 git push", "git push"},
		{"xargs -n 1 git push", "git push"},
		{"nohup git push", "git push"},
		{"stdbuf -oL git push", "git push"},
		{"stdbuf -o L git push", "git push"},
		{"sudo env A=1 nice timeout 5 nohup git push", "git push"},
		{"env", "env"},
		{"env A=1", "env A=1"},
		{"sudo", "sudo"},
	}
	for _, c := range cases {
		got := strings.Join(StripWrappers(strings.Fields(c.in)), " ")
		if got != c.want {
			t.Errorf("StripWrappers(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripWrappersSplitString(t *testing.T) {
	got := StripWrappers([]string{"env", "-S", "git push --force"})
	if !slices.Equal(got, []string{"git", "push", "--force"}) {
		t.Fatalf("env -S: got %q", got)
	}
}

func TestStripWrappersDepthBound(t *testing.T) {
	argv := make([]string, 0, 100)
	for range 99 {
		argv = append(argv, "nohup")
	}
	argv = append(argv, "git")
	if got := StripWrappers(argv); len(got) == 1 {
		t.Fatalf("stripping should stop at the depth bound, got %q", got)
	}
}

func gatesSpec() Spec {
	return Spec{Gates: []Gate{
		{Name: "force-push", On: KindExec, Match: GateMatch{Program: []string{"git"}, Args: []string{"push", "--force"}}},
		{Name: "shell", On: KindExec, Match: GateMatch{Shell: true}, Mode: ModeNotify},
		{Name: "rm", On: KindExec, Match: GateMatch{Program: []string{"rm"}}, Mode: ModeDeny, Reason: "no rm"},
		{Name: "destroy", On: KindLifecycle, Match: GateMatch{Action: []LifecycleAction{ActionDestroy, ActionMigrate}}, Mode: ModeDeny},
	}}
}

func TestEvaluateExec(t *testing.T) {
	s := gatesSpec()
	cases := []struct {
		argv  []string
		shell bool
		want  Mode
	}{
		{[]string{"git", "push", "--force"}, false, ModeAsk},
		{[]string{"git", "push", "origin", "main", "--force"}, false, ModeAsk},
		{[]string{"/usr/bin/git", "push", "--force"}, false, ModeAsk},
		{[]string{"env", "GIT_TRACE=1", "git", "push", "--force"}, false, ModeAsk},
		{[]string{"sudo", "git", "push", "--force"}, false, ModeAsk},
		{[]string{"git", "--force", "push"}, false, ""},
		{[]string{"git", "push"}, false, ""},
		{[]string{"sh", "-lc", "git push --force"}, true, ModeNotify},
		{[]string{"bash", "-c", "git push --force"}, false, ModeNotify},
		{[]string{"bash", "script.sh"}, false, ""},
		{[]string{"sudo", "rm", "-rf", "/"}, false, ModeDeny},
	}
	for _, c := range cases {
		got := s.EvaluateExec(ExecCall{Argv: c.argv, Shell: c.shell}).Mode()
		if got != c.want {
			t.Errorf("EvaluateExec(%q, shell=%v) = %q, want %q", c.argv, c.shell, got, c.want)
		}
	}
	if v := s.EvaluateExec(ExecCall{Argv: []string{"rm", "x"}}); v.Deny == nil || v.Deny.DenyReason() != "no rm" {
		t.Fatalf("deny reason: %+v", v.Deny)
	}
}

func TestEvaluateStrictestWins(t *testing.T) {
	s := Spec{Gates: []Gate{
		{Name: "watch", On: KindExec, Match: GateMatch{Program: []string{"git"}}, Mode: ModeNotify},
		{Name: "ask", On: KindExec, Match: GateMatch{Program: []string{"git"}}},
		{Name: "deny", On: KindExec, Match: GateMatch{Program: []string{"git"}, Args: []string{"push"}}, Mode: ModeDeny},
	}}
	if got := s.EvaluateExec(ExecCall{Argv: []string{"git", "status"}}); got.Mode() != ModeAsk || len(got.Notify) != 1 {
		t.Fatalf("git status: %+v", got)
	}
	if got := s.EvaluateExec(ExecCall{Argv: []string{"git", "push"}}).Mode(); got != ModeDeny {
		t.Fatalf("git push: %q", got)
	}
}

func TestEvaluateAllowlist(t *testing.T) {
	s := Spec{Exec: &ExecPolicy{Default: ModeAsk, Allow: []string{"git", "ls"}}}
	cases := []struct {
		argv  []string
		shell bool
		want  Mode
	}{
		{[]string{"git", "status"}, false, ""},
		{[]string{"sudo", "ls"}, false, ""},
		{[]string{"python", "-c", "print(1)"}, false, ModeAsk},
		{[]string{"sh", "-lc", "ls"}, true, ModeAsk},
	}
	for _, c := range cases {
		v := s.EvaluateExec(ExecCall{Argv: c.argv, Shell: c.shell})
		if v.Mode() != c.want {
			t.Errorf("allowlist %q: got %q, want %q", c.argv, v.Mode(), c.want)
		}
		if c.want == ModeAsk && v.Ask[0].Name != AllowlistGate {
			t.Errorf("allowlist %q: gate %q", c.argv, v.Ask[0].Name)
		}
	}
	s.Exec.Default = ModeDeny
	if got := s.EvaluateExec(ExecCall{Argv: []string{"curl", "x"}}).Mode(); got != ModeDeny {
		t.Fatalf("default deny: %q", got)
	}
}

func TestEvaluateLifecycle(t *testing.T) {
	s := gatesSpec()
	if s.EvaluateLifecycle(ActionDestroy).Mode() != ModeDeny {
		t.Fatal("destroy should be denied")
	}
	if s.EvaluateLifecycle(ActionSnapshot).Mode() != "" {
		t.Fatal("snapshot should not match")
	}
}

func TestValidateGates(t *testing.T) {
	if err := gatesSpec().Validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	bad := []Gate{
		{Name: "", On: KindExec, Match: GateMatch{Program: []string{"git"}}},
		{Name: "x", On: "computer", Match: GateMatch{}},
		{Name: "x", On: KindExec, Match: GateMatch{}},
		{Name: "x", On: KindExec, Match: GateMatch{Shell: true, Program: []string{"git"}}},
		{Name: "x", On: KindExec, Match: GateMatch{Program: []string{"/usr/bin/git"}}},
		{Name: "x", On: KindLifecycle, Match: GateMatch{Action: []LifecycleAction{"reboot"}}},
		{Name: "x", On: KindLifecycle, Match: GateMatch{}},
		{Name: "x", On: KindExec, Match: GateMatch{Program: []string{"git"}}, Mode: "block"},
		{Name: "x", On: KindExec, Match: GateMatch{Program: []string{"git"}}, Mode: ModeDeny, Grant: "env"},
		{Name: "x", On: KindExec, Match: GateMatch{Program: []string{"git"}}, Grant: "forever"},
		{Name: "x", On: KindExec, Match: GateMatch{Program: []string{"git"}}, TimeoutSeconds: 1},
		{Name: AllowlistGate, On: KindExec, Match: GateMatch{Program: []string{"git"}}},
	}
	for i, g := range bad {
		if err := (Spec{Gates: []Gate{g}}).Validate(); err == nil {
			t.Errorf("bad[%d] %+v: expected an error", i, g)
		}
	}
	dup := Spec{Gates: []Gate{gatesSpec().Gates[0], gatesSpec().Gates[0]}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate names: %v", err)
	}
	if err := (Spec{Exec: &ExecPolicy{Default: ModeNotify}}).Validate(); err == nil {
		t.Error("exec.default notify should be refused")
	}
	if err := (Spec{}).Validate(); err == nil {
		t.Error("an empty spec should be refused")
	}
}

func TestParseGrant(t *testing.T) {
	for raw, want := range map[string]GrantSpec{
		"":         {Scope: GrantOnce},
		"once":     {Scope: GrantOnce},
		"env":      {Scope: GrantEnv},
		"ttl:30m":  {Scope: GrantTTL, TTL: 30 * time.Minute},
		"ttl:1h0s": {Scope: GrantTTL, TTL: time.Hour},
	} {
		got, err := ParseGrant(raw)
		if err != nil || got != want {
			t.Errorf("ParseGrant(%q) = %+v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"forever", "ttl:", "ttl:abc", "ttl:48h", "ttl:0s"} {
		if _, err := ParseGrant(raw); err == nil {
			t.Errorf("ParseGrant(%q): expected an error", raw)
		}
	}
	if s := (GrantSpec{Scope: GrantTTL, TTL: 30 * time.Minute}).String(); s != "ttl:30m0s" {
		t.Errorf("String: %q", s)
	}
}

func TestGrantKeyAndActive(t *testing.T) {
	a := GrantKey("g", []string{"git", "push"}, nil)
	if a != GrantKey("g", []string{"git", "push"}, nil) {
		t.Fatal("grant key must be stable")
	}
	if a == GrantKey("g", []string{"git", "push", "-f"}, nil) || a == GrantKey("h", []string{"git", "push"}, nil) {
		t.Fatal("grant key must differ by gate and argv")
	}
	if a == GrantKey("g", []string{"git", "push"}, []byte("k")) {
		t.Fatal("a keyed grant key must differ from the unkeyed one")
	}
	now := time.Now()
	in := Intervention{ID: "int_1", EnvID: "vm", Gate: "g"}
	if _, ok := NewGrant(in, GrantSpec{Scope: GrantOnce}, a, now); ok {
		t.Fatal("once must not store a grant")
	}
	g, ok := NewGrant(in, GrantSpec{Scope: GrantTTL, TTL: time.Minute}, a, now)
	if !ok || !g.Active(now) || g.Active(now.Add(2*time.Minute)) || !strings.HasPrefix(g.ID, "grant_") {
		t.Fatalf("ttl grant: %+v", g)
	}
	env, _ := NewGrant(in, GrantSpec{Scope: GrantEnv}, a, now)
	if !env.Active(now.Add(1000 * time.Hour)) {
		t.Fatal("env grant must not expire")
	}
}

func TestSettled(t *testing.T) {
	in := Intervention{Kind: KindExec, Mode: ModeAsk, Exec: &ExecPayload{Argv: []string{"ls"}}, State: StatePending}
	if in.Settled() {
		t.Fatal("pending is not settled")
	}
	in.State = StateApproved
	in.Decision = &Decision{Outcome: OutcomeApprove}
	if in.Settled() {
		t.Fatal("approved without a result is not settled")
	}
	in.Result = &Result{}
	if !in.Settled() {
		t.Fatal("approved with a result is settled")
	}
	rej := Intervention{Kind: KindExec, Mode: ModeAsk, Exec: &ExecPayload{}, State: StateRejected, Decision: &Decision{Outcome: OutcomeReject}}
	if !rej.Settled() {
		t.Fatal("rejected is settled")
	}
	note := Intervention{Kind: KindExec, Mode: ModeNotify, Exec: &ExecPayload{}, State: StateApproved, Decision: &Decision{Outcome: OutcomeApprove}}
	if !note.Settled() {
		t.Fatal("a notify record is settled at once")
	}
	in.Decision.EditedArgv = []string{"echo"}
	if !slices.Equal(in.Argv(), []string{"echo"}) {
		t.Fatal("Argv should prefer the edit")
	}
}
