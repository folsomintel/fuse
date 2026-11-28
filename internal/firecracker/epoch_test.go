package firecracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// epochAgent is a host agent holding vm-1 at epoch hostEpoch. it records
// the X-Fuse-Epoch of every request it gets.
func epochAgent(t *testing.T, hostEpoch string) (*Provider, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Fuse-Epoch"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/vm" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"vms":[{"vm_id":"vm-1","epoch":` + hostEpoch + `}]}`))
		case r.URL.Path == "/v1/vm" || r.URL.Path == "/v1/vm/vm-1":
			_, _ = w.Write([]byte(`{"vm_id":"vm-1","epoch":` + hostEpoch + `}`))
		case r.URL.Path == "/v1/vm/vm-1/exec":
			_, _ = w.Write([]byte(`{"exit_code":0}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, Token: "t"}), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestHandleCarriesItsEpoch(t *testing.T) {
	p, seen := epochAgent(t, "5")
	ctx := orchestrator.WithVMEpoch(context.Background(), 5)
	env, err := p.Create(ctx, orchestrator.Spec{Name: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	// a later call through the handle is fenced without ctx naming the epoch.
	if _, err := env.Exec(context.Background(), []string{"true"}, orchestrator.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if got := orchestrator.EnvEpoch(env); got != 5 {
		t.Fatalf("handle epoch = %d, want 5", got)
	}
	want := []string{"POST /v1/vm 5", "POST /v1/vm/vm-1/exec 5", "DELETE /v1/vm/vm-1 5"}
	if got := seen(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("requests:\n got %q\nwant %q", got, want)
	}
}

func TestGetRefusesAnotherIncarnation(t *testing.T) {
	p, _ := epochAgent(t, "3")
	_, err := p.Get(orchestrator.WithVMEpoch(context.Background(), 9), "vm-1")
	var inc *orchestrator.IncarnationError
	if !errors.As(err, &inc) || inc.Host != 3 || inc.Want != 9 {
		t.Fatalf("err = %v, want an IncarnationError host 3 want 9", err)
	}
	// the same incarnation, or a caller that names none, gets the vm.
	for _, ctx := range []context.Context{orchestrator.WithVMEpoch(context.Background(), 3), context.Background()} {
		env, err := p.Get(ctx, "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		if got := orchestrator.EnvEpoch(env); got != 3 {
			t.Fatalf("handle epoch = %d, want 3", got)
		}
	}
}

func TestPreFencingAgentIsNotFenced(t *testing.T) {
	// an agent that reports no epoch matches any incarnation.
	p, _ := epochAgent(t, "0")
	if _, err := p.Get(orchestrator.WithVMEpoch(context.Background(), 9), "vm-1"); err != nil {
		t.Fatal(err)
	}
	envs, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := orchestrator.EnvEpoch(envs[0]); got != 0 {
		t.Fatalf("listed epoch = %d, want 0", got)
	}
}
