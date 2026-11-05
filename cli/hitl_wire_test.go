package main

import (
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// toSDKHitl is what `fuse up` sends; every part of the compiled policy has
// to survive it, or the orchestrator silently enforces less than the
// Fusefile says.
func TestToSDKHitlCarriesTheWholePolicy(t *testing.T) {
	out := toSDKHitl(&fusefile.HitlSpec{
		Ask:      true,
		Secret:   &fusefile.HitlSecretSpec{Targets: []fusefile.HitlSecretTarget{{Env: "GH_TOKEN", Label: "token"}}},
		Takeover: &fusefile.HitlTakeoverSpec{Via: []string{"terminal"}},
		Gates: []fusefile.HitlGateSpec{
			{Name: "force-push", On: "exec", Program: []string{"git"}, Args: []string{"push", "--force"}},
			{Name: "flailing", On: "stall", Failures: 3},
			{Name: "extend", On: "expiry", BeforeSeconds: 300},
		},
		Exec: &fusefile.HitlExecPolicy{Default: "ask", Allow: []string{"ls"}},
	})
	if out.Requests.Secret == nil || out.Requests.Secret.Targets[0].Env != "GH_TOKEN" || out.Requests.Takeover == nil {
		t.Fatalf("requests = %+v", out.Requests)
	}
	if len(out.Gates) != 3 || out.Gates[0].Match.Program[0] != "git" || out.Gates[1].Match.Failures != 3 || out.Gates[2].Match.BeforeSeconds != 300 {
		t.Fatalf("gates = %+v", out.Gates)
	}
	if out.Exec == nil || out.Exec.Allow[0] != "ls" {
		t.Fatalf("exec = %+v", out.Exec)
	}
}
