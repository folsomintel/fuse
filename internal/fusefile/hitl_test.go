package fusefile

import (
	"reflect"
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
		"hitl:\n  timeout: soon\n  requests:\n    ask: {}\n":                     "hitl.timeout",
		"hitl:\n  timeout: 2s\n  requests:\n    ask: {}\n":                       "between",
		"hitl:\n  timeout: 1500ms\n  requests:\n    ask: {}\n":                   "between",
		"hitl:\n  on_timeout: maybe\n  requests:\n    ask: {}\n":                 "hitl.on_timeout",
		"hitl:\n  timeout: 5m\n":                                                 "declare at least one request kind",
		"hitl:\n  requests:\n    ask: {}\n    secret: {targets: []}\n":           "hitl.requests.secret: not yet supported",
		"hitl:\n  gates:\n    - name: x\n  requests:\n    ask: {}\n":             "hitl.gates: not yet supported",
		"hitl:\n  notify:\n    - webhook: https://x\n  requests:\n    ask: {}\n": "hitl.notify: not yet supported",
		"hitl:\n  requests:\n    ask: {}\n    poll: {}\n":                        "poll",
	}
	for body, want := range cases {
		_, err := parseHitl(t, body)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to contain %q", body, err, want)
		}
	}
}
