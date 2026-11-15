package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// hitlAgent stands in for a host agent's exec endpoint: it runs a command
// once per idempotency key and replays the first answer to a repeat, as
// fc-agent does.
type hitlAgent struct {
	mu   sync.Mutex
	runs int
	keys []string
	seen map[string][]byte
	srv  *httptest.Server
}

func newHitlAgent(t *testing.T) *hitlAgent {
	a := &hitlAgent{seen: map[string][]byte{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(hostwire.IdempotencyHeader)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.keys = append(a.keys, key)
		out, ok := a.seen[key]
		if !ok || key == "" {
			a.runs++
			out = []byte(fmt.Sprintf(`{"stdout":"secret-output-%d"}`, a.runs))
			a.seen[key] = out
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *hitlAgent) state() (int, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs, append([]string(nil), a.keys...)
}

// hitlRows is the intervention rows the fleets of an endpoint and its
// restart share.
type hitlRows struct {
	mu      sync.Mutex
	plans   map[string]orchestrator.HitlRunPlan
	results map[string]hitl.Result
	expired map[string]int
	settled chan string
}

func newHitlRows() *hitlRows {
	return &hitlRows{
		plans:   map[string]orchestrator.HitlRunPlan{},
		results: map[string]hitl.Result{},
		expired: map[string]int{},
		settled: make(chan string, 16),
	}
}

func (r *hitlRows) result(id string) (hitl.Result, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.results[id]
	return res, ok
}

func (r *hitlRows) store(id string, res hitl.Result) {
	r.mu.Lock()
	r.results[id] = res
	r.mu.Unlock()
	r.settled <- id
}

// hitlFleet scripts the Intervention object's steps over hitlRows. an
// in-process run is an exec on the hitlAgent; block, when set, holds the run
// after the agent answered and before the result is written.
type hitlFleet struct {
	*snapFleet
	rows    *hitlRows
	agent   *hitlAgent
	client  *http.Client
	block   chan struct{}
	ran     chan struct{}
	mu      sync.Mutex
	calls   []string
	earlyBy time.Duration
}

func newHitlFleet(rows *hitlRows, agent *hitlAgent) *hitlFleet {
	return &hitlFleet{snapFleet: newSnapFleet(), rows: rows, agent: agent, client: hostwire.NewClient(), ran: make(chan struct{}, 4)}
}

func (f *hitlFleet) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *hitlFleet) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *hitlFleet) HitlRunBegin(_ context.Context, id string) (orchestrator.HitlRunPlan, error) {
	f.record("begin")
	if _, ok := f.rows.result(id); ok {
		return orchestrator.HitlRunPlan{}, nil
	}
	f.rows.mu.Lock()
	defer f.rows.mu.Unlock()
	return f.rows.plans[id], nil
}

func (f *hitlFleet) HitlRunInProcess(ctx context.Context, id string) error {
	f.record("run")
	if _, ok := f.rows.result(id); ok {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.agent.srv.URL+"/v1/vm/x/exec", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var out struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	f.ran <- struct{}{}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.rows.store(id, hitl.Result{Exec: &hitl.ExecResult{Stdout: out.Stdout}})
	return nil
}

func (f *hitlFleet) HitlRunFinish(_ context.Context, id string, res hitl.Result) error {
	f.record("finish")
	if _, ok := f.rows.result(id); ok {
		return nil
	}
	f.rows.store(id, res)
	return nil
}

func (f *hitlFleet) HitlExpire(_ context.Context, id string) (time.Duration, error) {
	f.record("expire")
	f.mu.Lock()
	early := f.earlyBy
	f.earlyBy = 0
	f.mu.Unlock()
	if early > 0 {
		return early, nil
	}
	f.rows.mu.Lock()
	f.rows.expired[id]++
	f.rows.mu.Unlock()
	f.rows.settled <- id
	return 0, nil
}

func uniqueIntervention(name string) string {
	return fmt.Sprintf("int-%s-%d", name, time.Now().UnixNano())
}

func waitSettled(t *testing.T, ctx context.Context, rows *hitlRows, id string) {
	t.Helper()
	for {
		select {
		case got := <-rows.settled:
			if got == id {
				return
			}
		case <-ctx.Done():
			t.Fatalf("intervention %s never settled", id)
		}
	}
}

// TestHitlRunSurvivesRestartBeforeRun stands in for kill -9 of the
// orchestrator right after an approval was sent and before the run started:
// restate holds the run while the endpoint is gone and delivers it to the
// restarted one, which runs it once. a second send for the same approval,
// as the restart's resume pass makes, starts nothing.
func TestHitlRunSurvivesRestartBeforeRun(t *testing.T) {
	rows := newHitlRows()
	agent := newHitlAgent(t)
	id := uniqueIntervention("before")
	rows.plans[id] = orchestrator.HitlRunPlan{Route: orchestrator.HitlRouteInProcess, EnvID: "fuse-a"}

	first := newHitlFleet(rows, agent)
	client, addr, stop := serveFleetAt(t, first, "")
	stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SendHitlRun(ctx, id); err != nil {
		t.Fatalf("send: %v", err)
	}
	second := newHitlFleet(rows, agent)
	_, _, _ = serveFleetAt(t, second, addr)
	waitSettled(t, ctx, rows, id)

	if err := client.SendHitlRun(ctx, id); err != nil {
		t.Fatalf("second send: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	runs, _ := agent.state()
	res, _ := rows.result(id)
	if runs != 1 || res.Exec == nil || res.Exec.Stdout != "secret-output-1" {
		t.Fatalf("agent ran %d times, result %+v; want one run and its output", runs, res)
	}
	if got := first.seen(); len(got) != 0 {
		t.Fatalf("the stopped endpoint ran %v", got)
	}
	if got := fmt.Sprint(second.seen()); got != "[begin run]" {
		t.Fatalf("restarted endpoint ran %s, want one begin and one run", got)
	}
}

// TestHitlRunExecNotRerunAfterRestart stands in for kill -9 while a held exec
// runs: the agent has run it, the orchestrator dies before writing the
// result, and the restarted endpoint retries the step. the retry carries the
// same idempotency key, so the agent replays its first answer and the command
// runs once. while the run is held, the journal has no trace of the output.
func TestHitlRunExecNotRerunAfterRestart(t *testing.T) {
	rows := newHitlRows()
	agent := newHitlAgent(t)
	id := uniqueIntervention("during")
	rows.plans[id] = orchestrator.HitlRunPlan{Route: orchestrator.HitlRouteInProcess, EnvID: "fuse-a"}

	first := newHitlFleet(rows, agent)
	first.block = make(chan struct{})
	defer close(first.block)
	client, addr, stop := serveFleetAt(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SendHitlRun(ctx, id); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-first.ran:
	case <-ctx.Done():
		t.Fatal("the exec never ran")
	}
	journal := readJournalOf(t, ctx, InterventionObject, id, "begin")
	if bytes.Contains(journal, []byte("secret-output")) {
		t.Fatalf("journal of %s holds the exec output", id)
	}

	stop()
	second := newHitlFleet(rows, agent)
	_, _, _ = serveFleetAt(t, second, addr)
	waitSettled(t, ctx, rows, id)

	runs, keys := agent.state()
	res, _ := rows.result(id)
	if runs != 1 || res.Exec == nil || res.Exec.Stdout != "secret-output-1" {
		t.Fatalf("agent ran %d times, result %+v; want one run and its output", runs, res)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("idempotency keys %q, want the same key on the run and its retry", keys)
	}
	if got := fmt.Sprint(second.seen()); got != "[run]" {
		t.Fatalf("restarted endpoint ran %s, want only the run step", got)
	}
}

// TestHitlExpireSurvivesRestart: a deadline timer armed before a restart
// fires on the restarted endpoint, and one that fires early by the
// orchestrator's clock arms itself again for the rest.
func TestHitlExpireSurvivesRestart(t *testing.T) {
	rows := newHitlRows()
	id := uniqueIntervention("expire")
	first := newHitlFleet(rows, nil)
	client, addr, stop := serveFleetAt(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SendHitlExpire(ctx, id, time.Second); err != nil {
		t.Fatalf("send: %v", err)
	}
	stop()
	time.Sleep(1500 * time.Millisecond)
	second := newHitlFleet(rows, nil)
	second.earlyBy = 300 * time.Millisecond
	_, _, _ = serveFleetAt(t, second, addr)
	waitSettled(t, ctx, rows, id)

	rows.mu.Lock()
	n := rows.expired[id]
	rows.mu.Unlock()
	if n != 1 {
		t.Fatalf("expired %d times, want once", n)
	}
	if got := first.seen(); len(got) != 0 {
		t.Fatalf("the stopped endpoint ran %v", got)
	}
	if got := fmt.Sprint(second.seen()); got != "[expire expire]" {
		t.Fatalf("restarted endpoint ran %s, want the early timer and its re-arm", got)
	}
}

// TestHitlRunSnapshotCallsEnvironment: an approved snapshot is taken by the
// Environment object's own handler, and the run writes the snapshot id it
// returned.
func TestHitlRunSnapshotCallsEnvironment(t *testing.T) {
	rows := newHitlRows()
	id := uniqueIntervention("snap")
	rows.plans[id] = orchestrator.HitlRunPlan{Route: orchestrator.HitlRouteSnapshot, EnvID: uniqueVM("hitl-snap"),
		Snapshot: &orchestrator.SnapshotOptions{Comment: "approved"}}
	fleet := newHitlFleet(rows, nil)
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SendHitlRun(ctx, id); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitSettled(t, ctx, rows, id)
	res, _ := rows.result(id)
	if res.Error != "" || res.SnapshotID != "snap-1" {
		t.Fatalf("result %+v, want snap-1", res)
	}
	if got, want := fleet.steps(), "begin,checkpoint,record,list,ready"; got != want {
		t.Fatalf("snapshot steps %s, want %s", got, want)
	}
	if got := fmt.Sprint(fleet.seen()); got != "[begin finish]" {
		t.Fatalf("run steps %s, want begin and finish", got)
	}
}
