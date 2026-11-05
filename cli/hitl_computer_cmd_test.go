package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
)

func TestToSDKHitlCarriesComputerGatesDecidersAndReview(t *testing.T) {
	out := toSDKHitl(&fusefile.HitlSpec{
		Review:   true,
		Takeover: &fusefile.HitlTakeoverSpec{Via: []string{"desktop"}},
		Gates: []fusefile.HitlGateSpec{
			{Name: "checkout", On: "computer", Action: []string{"type"}, TextContains: []string{"card"},
				Decide: &fusefile.HitlDecider{URL: "https://policy.example/fuse", Secret: "DECIDE_KEY"}},
		},
	})
	if out.Requests.Review == nil || out.Requests.Takeover.Via[0] != "desktop" {
		t.Fatalf("requests = %+v", out.Requests)
	}
	g := out.Gates[0]
	if g.Match.TextContains[0] != "card" || g.Decide == nil || g.Decide.Secret != "DECIDE_KEY" {
		t.Fatalf("gate = %+v", g)
	}
}

func TestHitlApproveWithText(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		fmt.Fprint(w, `{"id":"int_1","state":"approved","decision":{"outcome":"approve","edited_text":"ls -la","grant":"once"}}`)
	}))
	defer srv.Close()

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "approve", "int_1", "--text", "ls -la"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if body["edited_text"] != "ls -la" {
		t.Fatalf("body = %v", body)
	}
}

func TestHitlShowRendersAReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"int_r","env_id":"vm-1","kind":"review","state":"pending","payload":{"review":{"path":"/work/fix.diff","content":"diff --git a/x b/x","format":"diff","message":"please look"}}}`)
	}))
	defer srv.Close()
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "show", "int_r"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"diff from /work/fix.diff", "please look", "diff --git a/x b/x"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show is missing %q:\n%s", want, out)
		}
	}
}
