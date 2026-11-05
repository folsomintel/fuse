package fusefile

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func parseHitl(t *testing.T, body string) (*Fusefile, error) {
	t.Helper()
	return Parse([]byte("version: 1\nrun: echo hi\n" + body))
}

func TestHitlCompiles(t *testing.T) {
	f, err := parseHitl(t, `hitl:
  timeout: 2m
  on_timeout: allow
  requests:
    ask: {}
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := &HitlSpec{TimeoutSeconds: 120, OnTimeout: "allow", Ask: true}
	if !reflect.DeepEqual(c.Hitl, want) {
		t.Fatalf("hitl = %+v, want %+v", c.Hitl, want)
	}
}

func TestHitlDefaults(t *testing.T) {
	f, err := parseHitl(t, "hitl:\n  requests:\n    ask: {}\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, _ := Compile(f)
	if c.Hitl == nil || c.Hitl.TimeoutSeconds != 0 || c.Hitl.OnTimeout != "reject" || !c.Hitl.Ask {
		t.Fatalf("defaults = %+v", c.Hitl)
	}
}

func TestHitlAbsentStaysNil(t *testing.T) {
	f, err := parseHitl(t, "")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Compile(f)
	if c.Hitl != nil {
		t.Fatalf("no block compiled to %+v", c.Hitl)
	}
}

func TestHitlRejects(t *testing.T) {
	cases := map[string]string{
		"hitl:\n  timeout: soon\n  requests:\n    ask: {}\n":                                                                                     "hitl.timeout",
		"hitl:\n  timeout: 2s\n  requests:\n    ask: {}\n":                                                                                       "between",
		"hitl:\n  timeout: 1500ms\n  requests:\n    ask: {}\n":                                                                                   "between",
		"hitl:\n  on_timeout: maybe\n  requests:\n    ask: {}\n":                                                                                 "hitl.on_timeout",
		"hitl:\n  timeout: 5m\n":                                                                                                                 "declare at least one of requests (ask, secret, review or takeover), gates or exec",
		"hitl:\n  requests:\n    secret: {targets: []}\n":                                                                                        "declare at least one target",
		"hitl:\n  requests:\n    secret: {targets: [{env: GH_TOKEN}]}\n":                                                                         "label is required",
		"hitl:\n  requests:\n    secret: {targets: [{env: 1BAD, label: x}]}\n":                                                                   "not a valid variable name",
		"hitl:\n  requests:\n    secret: {targets: [{file: rel/path, label: x}]}\n":                                                              "absolute path",
		"hitl:\n  requests:\n    secret: {targets: [{file: /fuse/env, label: x}]}\n":                                                             "reserves",
		"hitl:\n  requests:\n    secret: {targets: [{env: A, file: /a, label: x}]}\n":                                                            "not both",
		"hitl:\n  requests:\n    secret: {targets: [{env: A, label: x}, {env: A, label: y}]}\n":                                                  "declared twice",
		"hitl:\n  requests:\n    takeover: {via: [desktop]}\n":                                                                                   "desktop needs a desktop block",
		"hitl:\n  requests:\n    takeover: {via: [port]}\n":                                                                                      "not yet supported",
		"hitl:\n  gates:\n    - {name: x, on: computer, match: {}}\n":                                                                            "a computer gate needs action or text_contains",
		"hitl:\n  gates:\n    - {name: x, on: computer, match: {action: pay}}\n":                                                                 "must be one of screenshot",
		"hitl:\n  gates:\n    - {name: x, on: computer, match: {action: key, text_contains: pw}}\n":                                              "only applies to the type action",
		"hitl:\n  gates:\n    - {name: x, on: exec, match: {program: git, text_contains: pw}}\n":                                                 "only applies to on: computer",
		"hitl:\n  gates:\n    - {name: x, on: computer, match: {action: type}, decide: {url: https://d}}\n":                                      "decide.secret: name the secret",
		"hitl:\n  gates:\n    - {name: x, on: computer, match: {action: type}, decide: {url: https://d, secret: S}}\n":                           "must also be listed under the top-level secrets",
		"secrets: [S]\nhitl:\n  gates:\n    - {name: x, on: computer, mode: deny, match: {action: type}, decide: {url: https://d, secret: S}}\n": "only an ask gate can have a decider",
		"secrets: [S]\nhitl:\n  gates:\n    - {name: x, on: stall, match: {failures: 2}, decide: {url: https://d, secret: S}}\n":                 "no action for a decider",
		"hitl:\n  gates:\n    - {name: x, on: exec, match: {}}\n":                                                                                "needs program, args or shell",
		"hitl:\n  gates:\n    - {name: x, on: exec, match: {program: /bin/git}}\n":                                                               "not a path",
		"hitl:\n  gates:\n    - {name: x, on: lifecycle, match: {action: reboot}}\n":                                                             "must be one of destroy",
		"hitl:\n  gates:\n    - {name: x, on: exec, mode: deny, grant: env, match: {program: git}}\n":                                            "only an ask gate can grant",
		"hitl:\n  gates:\n    - {name: x, on: exec, grant: forever, match: {program: git}}\n":                                                    "must be once, env or ttl",
		"hitl:\n  gates:\n    - {name: x, on: exec, match: {program: git}}\n    - {name: x, on: exec, match: {program: rm}}\n":                   "duplicate",
		"hitl:\n  exec: {default: notify, allow: [git]}\n":                                                                                       "hitl.exec.default",
		"hitl:\n  notify:\n    - webhook: https://x\n  requests:\n    ask: {}\n":                                                                 "hitl.notify[0].secret: name the secret",
		"hitl:\n  notify:\n    - {webhook: https://x, secret: S}\n  requests:\n    ask: {}\n":                                                    "must also be listed under the top-level secrets",
		"secrets: [S]\nhitl:\n  notify:\n    - {webhook: ftp://x, secret: S}\n  requests:\n    ask: {}\n":                                        "hitl.notify[0].webhook",
		"hitl:\n  requests:\n    ask: {}\n    poll: {}\n":                                                                                        "poll",
	}
	for body, want := range cases {
		_, err := parseHitl(t, body)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to contain %q", body, err, want)
		}
	}
}

func TestHitlNotifyAndRequireActorCompile(t *testing.T) {
	f, err := parseHitl(t, `secrets: [HOOK_SECRET]
hitl:
  require_actor: true
  notify:
    - webhook: https://hooks.example.com/fuse
      secret: HOOK_SECRET
  requests:
    ask: {}
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := &HitlSpec{OnTimeout: "reject", Ask: true, RequireActor: true,
		Notify: []HitlWebhook{{URL: "https://hooks.example.com/fuse", Secret: "HOOK_SECRET"}}}
	if !reflect.DeepEqual(c.Hitl, want) {
		t.Fatalf("hitl = %+v, want %+v", c.Hitl, want)
	}
	if !slices.Contains(c.RequiredSecrets, "HOOK_SECRET") {
		t.Fatalf("required secrets %v miss the webhook secret", c.RequiredSecrets)
	}
}

func TestHitlGatesCompile(t *testing.T) {
	f, err := parseHitl(t, `hitl:
  exec:
    default: ask
    allow: [git, ls]
  gates:
    - name: force-push
      on: exec
      match: { program: git, args: [push, --force] }
      grant: ttl:30m
      timeout: 2m
    - name: opaque
      on: exec
      match: { shell: true }
      mode: notify
    - name: no-destroy
      on: lifecycle
      match: { action: [destroy, migrate] }
      mode: deny
      reason: not yours
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := &HitlSpec{
		OnTimeout: "reject",
		Exec:      &HitlExecPolicy{Default: "ask", Allow: []string{"git", "ls"}},
		Gates: []HitlGateSpec{
			{Name: "force-push", On: "exec", Program: []string{"git"}, Args: []string{"push", "--force"}, Grant: "ttl:30m", TimeoutSeconds: 120},
			{Name: "opaque", On: "exec", Shell: true, Mode: "notify"},
			{Name: "no-destroy", On: "lifecycle", Action: []string{"destroy", "migrate"}, Mode: "deny", Reason: "not yours"},
		},
	}
	if !reflect.DeepEqual(c.Hitl, want) {
		t.Fatalf("hitl = %+v, want %+v", c.Hitl, want)
	}
}

func TestHitlSecretAndTakeoverCompile(t *testing.T) {
	f, err := parseHitl(t, `hitl:
  requests:
    takeover: {via: [terminal]}
    secret:
      targets:
        - { env: GH_TOKEN, label: "GitHub token for the deploy bot" }
        - { file: /root/.netrc, label: "netrc for the private registry" }
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := &HitlSpec{OnTimeout: "reject",
		Secret: &HitlSecretSpec{Targets: []HitlSecretTarget{
			{Env: "GH_TOKEN", Label: "GitHub token for the deploy bot"},
			{File: "/root/.netrc", Label: "netrc for the private registry"},
		}},
		Takeover: &HitlTakeoverSpec{Via: []string{"terminal"}},
	}
	if !reflect.DeepEqual(c.Hitl, want) {
		t.Fatalf("hitl = %+v, want %+v", c.Hitl, want)
	}
}

func TestHitlSignalGatesCompile(t *testing.T) {
	f, err := parseHitl(t, `hitl:
  requests:
    ask: {}
  gates:
    - { name: flailing, on: stall, match: { failures: 3 } }
    - { name: quiet, on: stall, mode: notify, match: { quiet: 15m } }
    - { name: extend, on: expiry, match: { before: 5m } }
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []HitlGateSpec{
		{Name: "flailing", On: "stall", Failures: 3},
		{Name: "quiet", On: "stall", Mode: "notify", QuietSeconds: 900},
		{Name: "extend", On: "expiry", BeforeSeconds: 300},
	}
	if !reflect.DeepEqual(c.Hitl.Gates, want) {
		t.Fatalf("gates = %+v, want %+v", c.Hitl.Gates, want)
	}
}

func TestHitlSignalGatesReject(t *testing.T) {
	cases := map[string]string{
		"- { name: s, on: stall, match: {} }":                           "needs failures or quiet",
		"- { name: s, on: stall, match: { quiet: 5s } }":                "must be between 1m0s",
		"- { name: s, on: stall, mode: deny, match: { failures: 1 } }":  "no action to deny",
		"- { name: s, on: stall, match: { failures: 1, before: 5m } }":  "only applies to on: expiry",
		"- { name: e, on: expiry, match: {} }":                          "needs before",
		"- { name: e, on: expiry, grant: env, match: { before: 5m } }":  "cannot grant",
		"- { name: x, on: exec, match: { program: git, failures: 2 } }": "only apply to on: stall or expiry",
	}
	for gate, want := range cases {
		_, err := parseHitl(t, "hitl:\n  requests:\n    ask: {}\n  gates:\n    "+gate+"\n")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", gate, err, want)
		}
	}
}

func TestHitlComputerReviewAndDeciderCompile(t *testing.T) {
	f, err := parseHitl(t, `secrets: [DECIDE_KEY]
desktop: {width: 1280, height: 800}
hitl:
  requests:
    review: {}
    takeover: {via: [terminal, desktop]}
  gates:
    - name: checkout
      on: computer
      match: {action: [type, key]}
      decide: {url: "https://policy.example.com/fuse", secret: DECIDE_KEY}
    - name: passwords
      on: computer
      match: {text_contains: password}
      mode: deny
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	h := c.Hitl
	if !h.Review || !slices.Equal(h.Takeover.Via, []string{"terminal", "desktop"}) || len(h.Gates) != 2 {
		t.Fatalf("hitl = %+v", h)
	}
	g := h.Gates[0]
	if g.On != "computer" || !slices.Equal(g.Action, []string{"type", "key"}) || g.Decide == nil || g.Decide.Secret != "DECIDE_KEY" {
		t.Fatalf("gate 0 = %+v", g)
	}
	if !slices.Equal(h.Gates[1].TextContains, []string{"password"}) {
		t.Fatalf("gate 1 = %+v", h.Gates[1])
	}
}
