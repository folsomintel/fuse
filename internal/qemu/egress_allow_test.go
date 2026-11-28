package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/egress"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestEgressWireFor_Allow pins the create-time egress field: plain direct
// sends no field at all, as before the allowlist, a direct allowlist sends
// the mode with it, and proxy keeps sending its mode.
func TestEgressWireFor_Allow(t *testing.T) {
	plain, err := json.Marshal(createVMRequest{Name: "a", Egress: egressWireFor(orchestrator.Spec{})})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "egress") {
		t.Fatalf("plain direct create carries egress: %s", plain)
	}
	if w := egressWireFor(orchestrator.Spec{Egress: egress.Spec{Mode: egress.ModeDirect, Allow: []string{}}}); w != nil {
		t.Fatalf("empty allowlist produced egress wire %+v, want nil", w)
	}

	allow := []string{"10.0.0.0/8", "1.2.3.4/32"}
	got, err := json.Marshal(egressWireFor(orchestrator.Spec{Egress: egress.Spec{Allow: allow}}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"mode":"direct","allow":["10.0.0.0/8","1.2.3.4/32"]}`; string(got) != want {
		t.Fatalf("direct allowlist wire: got %s, want %s", got, want)
	}

	got, err = json.Marshal(egressWireFor(orchestrator.Spec{Egress: egress.Spec{Mode: egress.ModeProxy, Provider: "mock"}}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"mode":"proxy"}`; string(got) != want {
		t.Fatalf("proxy wire: got %s, want %s", got, want)
	}
}

// TestSetEgressAllow pins the agent call: a PUT of the list, the agent's
// normalized list back, and the agent's refusals as status errors.
func TestSetEgressAllow(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "bad request shape", http.StatusTeapot)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch r.URL.Path {
		case "/v1/vm/vm-1/egress/allow":
			_, _ = w.Write([]byte(`{"cidrs":["1.2.3.4/32"]}`))
		case "/v1/vm/missing/egress/allow":
			http.Error(w, "vm not found", http.StatusNotFound)
		case "/v1/vm/proxied/egress/allow":
			http.Error(w, "proxy egress", http.StatusConflict)
		case "/v1/vm/bad/egress/allow":
			http.Error(w, "not an ipv4 cidr", http.StatusBadRequest)
		default:
			http.Error(w, "unexpected path", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Token: "token"})
	ctx := context.Background()

	got, err := p.SetEgressAllow(ctx, "vm-1", []string{"1.2.3.4"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"1.2.3.4/32"}) {
		t.Fatalf("normalized list: got %v", got)
	}
	if gotBody != `{"cidrs":["1.2.3.4"]}` {
		t.Fatalf("body: got %s", gotBody)
	}

	// nil lifts the list; it must reach the agent as [] and not null.
	if _, err := p.SetEgressAllow(ctx, "vm-1", nil); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if gotBody != `{"cidrs":[]}` {
		t.Fatalf("nil body: got %s", gotBody)
	}

	for vmID, code := range map[string]int{"missing": 404, "bad": 400} {
		_, err := p.SetEgressAllow(ctx, vmID, []string{"1.2.3.4"})
		var statusErr *orchestrator.HTTPStatusError
		if !errors.As(err, &statusErr) || statusErr.Code != code {
			t.Fatalf("%s: got %v, want an HTTPStatusError %d", vmID, err, code)
		}
		if orchestrator.IsNotFound(err) != (code == 404) {
			t.Fatalf("%s: IsNotFound = %v", vmID, orchestrator.IsNotFound(err))
		}
	}
}

func TestSetEgressAllow_StubRefuses(t *testing.T) {
	var setter orchestrator.EgressAllowSetter = New(Config{})
	if _, err := setter.SetEgressAllow(context.Background(), "vm-1", nil); err == nil {
		t.Fatal("stub claimed to enforce an allowlist")
	}
}
