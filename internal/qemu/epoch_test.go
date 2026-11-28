package qemu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// the qemu handle is fenced like the firecracker one: it carries the
// incarnation it was made under, and Get refuses a vm of another.
func TestHandleCarriesItsEpoch(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Fuse-Epoch"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/vm/vm-1/exec" {
			_, _ = w.Write([]byte(`{"exit_code":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"vm_id":"vm-1","epoch":4}`))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Token: "t"})

	env, err := p.Create(orchestrator.WithVMEpoch(context.Background(), 4), orchestrator.Spec{Name: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Exec(context.Background(), []string{"true"}, orchestrator.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := orchestrator.EnvEpoch(env); got != 4 {
		t.Fatalf("handle epoch = %d, want 4", got)
	}
	_, err = p.Get(orchestrator.WithVMEpoch(context.Background(), 8), "vm-1")
	var inc *orchestrator.IncarnationError
	if !errors.As(err, &inc) || inc.Host != 4 || inc.Want != 8 {
		t.Fatalf("err = %v, want an IncarnationError host 4 want 8", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"POST /v1/vm 4", "POST /v1/vm/vm-1/exec 4", "GET /v1/vm/vm-1 "}
	if len(seen) != len(want) {
		t.Fatalf("requests:\n got %q\nwant %q", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("requests:\n got %q\nwant %q", seen, want)
		}
	}
}
