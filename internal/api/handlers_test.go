package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TODO: Add test cases for invalid/edge-case inputs: negative CPU counts,
// empty task IDs on non-create endpoints, malformed JSON bodies, oversized
// payloads. Current tests cover happy paths but not input validation boundaries.

// TODO: Add tests for metrics middleware — verify that route labels use chi
// patterns (not raw URLs) and that status codes are recorded correctly.

// ── Test doubles ──────────────────────────────────────────────────
//
// These mirror the fixtures in fleet_test.go but live in the api
// package so the handler layer can be tested in isolation. They are
// intentionally minimal — any behavior the tests don't exercise is
// stubbed to a happy-path return.

type fakeEnv struct {
	name string
	url  string

	mu          sync.Mutex
	checkpoints []orchestrator.Checkpoint
	execCalls   [][]string

	// execResult and execErr program what Exec returns, so a test can drive
	// both a clean run and a guest command that exits non-zero — which are
	// different outcomes the handler must not conflate.
	execResult orchestrator.ExecResult
	execErr    error

	// execArgv and execOpts record what Exec was actually asked to run.
	execArgv [][]string
	execOpts []orchestrator.ExecOptions

	// execDelay makes Exec block before returning, to stand in for a
	// long-running guest command. Read without the lock so it must be set
	// before the call.
	execDelay time.Duration

	// attachStream is handed back by Attach; attachErr overrides it.
	// attachSpec records what the handler asked for.
	attachStream io.ReadWriteCloser
	attachErr    error
	attachSpec   orchestrator.AttachSpec
	attachCalls  int
}

func (e *fakeEnv) Name() string  { return e.name }
func (e *fakeEnv) URL() string   { return e.url }
func (e *fakeEnv) Token() string { return "" }

func (e *fakeEnv) Exec(_ context.Context, cmd []string, opts orchestrator.ExecOptions) (orchestrator.ExecResult, error) {
	e.mu.Lock()
	e.execArgv = append(e.execArgv, cmd)
	e.execOpts = append(e.execOpts, opts)
	res, err, delay := e.execResult, e.execErr, e.execDelay
	e.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return res, err
}

func (e *fakeEnv) Attach(_ context.Context, spec orchestrator.AttachSpec) (io.ReadWriteCloser, error) {
	e.mu.Lock()
	e.attachSpec = spec
	e.attachCalls++
	stream, err := e.attachStream, e.attachErr
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (e *fakeEnv) ExecStream(_ context.Context, _, _ io.Writer, name string, args ...string) error {
	e.mu.Lock()
	e.execCalls = append(e.execCalls, append([]string{name}, args...))
	e.mu.Unlock()
	return nil
}

// execInvocations returns a copy of the argv recorded by Exec (as opposed to
// execCommands, which records the ExecStream calls drain makes).
func (e *fakeEnv) execInvocations() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]string, len(e.execArgv))
	copy(out, e.execArgv)
	return out
}

// execCommands returns a copy of the recorded ExecStream calls so drain tests
// can assert the configured DrainCommand actually ran.
func (e *fakeEnv) execCommands() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]string, len(e.execCalls))
	copy(out, e.execCalls)
	return out
}

func (e *fakeEnv) Upload(context.Context, []byte, string) error { return nil }
func (e *fakeEnv) StartAgent(context.Context, orchestrator.AgentSpec) error {
	return nil
}
func (e *fakeEnv) Checkpoint(context.Context, string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := fmt.Sprintf("cp-%d", len(e.checkpoints)+1)
	e.checkpoints = append(e.checkpoints, orchestrator.Checkpoint{
		ID:        id,
		SizeBytes: int64(128*len(e.checkpoints) + 128),
	})
	return id, nil
}
func (e *fakeEnv) Restore(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, checkpoint := range e.checkpoints {
		if checkpoint.ID == checkpointID {
			return nil
		}
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *fakeEnv) DeleteCheckpoint(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, checkpoint := range e.checkpoints {
		if checkpoint.ID != checkpointID {
			continue
		}
		e.checkpoints = append(e.checkpoints[:i], e.checkpoints[i+1:]...)
		return nil
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *fakeEnv) ListCheckpoints(context.Context) ([]orchestrator.Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]orchestrator.Checkpoint, len(e.checkpoints))
	copy(out, e.checkpoints)
	return out, nil
}

type fakeProvider struct {
	mu   sync.Mutex
	envs map[string]*fakeEnv
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{envs: make(map[string]*fakeEnv)}
}

func (p *fakeProvider) Create(_ context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := &fakeEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *fakeProvider) Get(_ context.Context, name string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("not found: %s", name)
	}
	return e, nil
}

func (p *fakeProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, name)
	return nil
}

func (p *fakeProvider) List(_ context.Context, _ string) ([]orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]orchestrator.Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (*fakeProvider) Close() error { return nil }

// ── Helpers ───────────────────────────────────────────────────────

// newTestHandler wires a real FleetManager with an in-memory state
// store against the fake provider. The returned handler is ready to
// serve requests; the test owns the mux returned by Router().
func newTestHandler(t *testing.T) (*Handler, *orchestrator.FleetManager, *fakeProvider) {
	t.Helper()
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	return &Handler{Fleet: fm}, fm, p
}

// encodeManifest base64-encodes a small valid-looking manifest body.
// The fake provider never actually parses it, but we send real bytes
// so the resolver path is exercised.
func encodeManifest(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(`{"version":"1","services":{}}`))
}

// decodeError pulls an Error envelope out of a response body.
func decodeError(t *testing.T, body io.Reader) Error {
	t.Helper()
	var e Error
	if err := json.NewDecoder(body).Decode(&e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return e
}

// doJSON builds an http.Request with a JSON body and serves it
// against the given handler, returning the response recorder.
func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// mustRouter calls h.Router() and fails the test if it returns an error.
func mustRouter(t *testing.T, h *Handler) http.Handler {
	t.Helper()
	r, err := h.Router()
	if err != nil {
		t.Fatalf("Router(): %v", err)
	}
	return r
}

// ── Environment tests ─────────────────────────────────────────────

func TestCreateEnvironment_happyPath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024},
		ManifestInline: encodeManifest(t),
	})

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.ID != "fuse-task-1" {
		t.Errorf("id = %q, want fuse-task-1", env.ID)
	}
	if env.State != "running" {
		t.Errorf("state = %q, want running", env.State)
	}
	if env.TaskID != "task-1" {
		t.Errorf("task_id = %q", env.TaskID)
	}
}

func TestCreateEnvironment_duplicateTaskReturns409(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	body := CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}
	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", body); rr.Code != http.StatusCreated {
		t.Fatalf("first call status = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeConflict {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeConflict)
	}
}

func TestCreateEnvironment_missingManifestUsesDefault(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-1",
		// ManifestInline intentionally omitted
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
}

func TestCreateEnvironment_missingTaskIDReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_invalidBase64Returns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: "not base64!",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestGetEnvironment_notFoundReturns404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-missing", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeNotFound {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestGetEnvironment_happyPath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	// Provision first.
	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-task-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var env Environment
	_ = json.NewDecoder(rr.Body).Decode(&env)
	if env.ID != "fuse-task-1" {
		t.Errorf("id = %q", env.ID)
	}
}

func TestListEnvironments_filtersByTaskID(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodGet, "/v1/environments?task_id=task-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var out EnvironmentList
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Environments) != 1 {
		t.Fatalf("len = %d, want 1", len(out.Environments))
	}
	if out.Environments[0].TaskID != "task-1" {
		t.Errorf("task_id = %q", out.Environments[0].TaskID)
	}
}

func TestDestroyEnvironment_returns204AndMissingReturns404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodDelete, "/v1/environments/fuse-task-1", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204. body: %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, r, http.MethodDelete, "/v1/environments/does-not-exist", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestListEnvironments_returnsAllTracked(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	for _, id := range []string{"a", "b", "c"} {
		_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
			TaskID:         id,
			ManifestInline: encodeManifest(t),
		})
	}

	rr := doJSON(t, r, http.MethodGet, "/v1/environments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var out EnvironmentList
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Environments) != 3 {
		t.Errorf("got %d envs, want 3", len(out.Environments))
	}
}

// ── Snapshot tests ────────────────────────────────────────────────

func TestSnapshotLifecycle_createListRestore(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	// Provision VM.
	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	// Create snapshot.
	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{
		Comment: "before-migration",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d. body: %s", rr.Code, rr.Body.String())
	}
	var snap Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&snap)
	if snap.ID != "cp-1" {
		t.Errorf("snapshot id = %q, want cp-1", snap.ID)
	}
	if snap.VMID != "fuse-task-1" {
		t.Errorf("vm_id = %q", snap.VMID)
	}
	if snap.Comment != "before-migration" {
		t.Errorf("comment = %q", snap.Comment)
	}
	if snap.State != "ready" {
		t.Errorf("state = %q, want ready", snap.State)
	}

	// List snapshots.
	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots?vm_id=fuse-task-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	var list SnapshotList
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list.Snapshots) != 1 {
		t.Fatalf("len = %d, want 1", len(list.Snapshots))
	}
	if list.Snapshots[0].ID != "cp-1" {
		t.Errorf("id = %q", list.Snapshots[0].ID)
	}

	// Get snapshot.
	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots/cp-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d", rr.Code)
	}

	// Restore snapshot.
	rr = doJSON(t, r, http.MethodPost, "/v1/snapshots/cp-1?action=restore", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("restore status = %d. body: %s", rr.Code, rr.Body.String())
	}
}

func TestSnapshot_restoreUnknownSnapshotReturns404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/snapshots/never-existed?action=restore", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshot_listForUnknownVMReturnsEmptyList(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodGet, "/v1/snapshots?vm_id=nope", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out SnapshotList
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Snapshots) != 0 {
		t.Fatalf("len = %d, want 0", len(out.Snapshots))
	}
}

func TestSnapshot_deleteLifecycle(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})
	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{
		Comment: "delete-me",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rr.Code)
	}

	rr = doJSON(t, r, http.MethodDelete, "/v1/snapshots/cp-1", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d. body: %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots/cp-1", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", rr.Code)
	}
}

func TestSnapshot_quotaExceededReturns409(t *testing.T) {
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:              p,
		Prefix:                "fuse-",
		SnapshotQuotaMaxCount: 1,
	})
	h := &Handler{Fleet: fm}
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{}); rr.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("quota status = %d, want 409", rr.Code)
	}
}

// ── Resolver tests ────────────────────────────────────────────────

func TestInlineResolver_defaultsEmpty(t *testing.T) {
	got, err := InlineResolver{}.Resolve(CreateEnvironmentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(defaultManifest) {
		t.Errorf("got %q, want default manifest", string(got))
	}
}

func TestInlineResolver_decodesBase64(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("hello"))
	got, err := InlineResolver{}.Resolve(CreateEnvironmentRequest{ManifestInline: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestInlineResolver_rejectsInvalidBase64(t *testing.T) {
	_, err := InlineResolver{}.Resolve(CreateEnvironmentRequest{ManifestInline: "!!!"})
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

// ── Drain action tests ────────────────────────────────────────────
//
// Drain delegates to FleetManager.Drain, which runs the configured
// DrainCommand inside the guest via Environment.ExecStream. Provisioning goes
// through the real ProvisionAndAssign → Boot → FusedAgentSpec path, so the VM
// picks up DefaultFusedDrainCommand automatically. These tests assert the HTTP
// contract and that the command was Exec'd on the env the Fleet actually holds.

// drainExecCount returns how many ExecStream calls the provisioned env
// recorded, asserting the env exists.
func drainExecCount(t *testing.T, p *fakeProvider, name string) int {
	t.Helper()
	p.mu.Lock()
	e, ok := p.envs[name]
	p.mu.Unlock()
	if !ok {
		t.Fatalf("fake env %q not found", name)
	}
	return len(e.execCommands())
}

func TestEnvironmentAction_Drain_HappyPath(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("drain status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}

	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if env.State != "draining" {
		t.Errorf("state = %q, want draining", env.State)
	}
	if env.ID != "fuse-task-1" {
		t.Errorf("id = %q", env.ID)
	}
	if got := drainExecCount(t, p, "fuse-task-1"); got != 1 {
		t.Errorf("drain command exec count = %d, want 1", got)
	}
}

func TestEnvironmentAction_Drain_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-missing?action=drain", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", rr.Code, rr.Body.String())
	}
}

func TestEnvironmentAction_Drain_AlreadyDrainingReturns409(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rr.Code)
	}

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil); rr.Code != http.StatusOK {
		t.Fatalf("first drain status = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("second drain status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	// The rejected second drain must not run the command again.
	if got := drainExecCount(t, p, "fuse-task-1"); got != 1 {
		t.Errorf("drain command exec count = %d, want 1 (second drain must not exec)", got)
	}
}

func TestEnvironmentAction_Drain_ThenDeleteSucceeds(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create: %d", rr.Code)
	}
	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil); rr.Code != http.StatusOK {
		t.Fatalf("drain: %d", rr.Code)
	}
	if rr := doJSON(t, r, http.MethodDelete, "/v1/environments/fuse-task-1", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete after drain: %d body=%s", rr.Code, rr.Body.String())
	}
}

// ── Fork action ───────────────────────────────────────────────────

// CreateFromCheckpoint makes fakeProvider satisfy orchestrator.SnapshotForkable
// so the fork happy path is exercisable end to end: it seeds a new env from the
// source env's matching checkpoint.
func (p *fakeProvider) CreateFromCheckpoint(_ context.Context, spec orchestrator.Spec, srcVMID, checkpointID string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	src, ok := p.envs[srcVMID]
	if !ok {
		return nil, fmt.Errorf("source env not found: %s", srcVMID)
	}
	e := &fakeEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	src.mu.Lock()
	for _, cp := range src.checkpoints {
		if cp.ID == checkpointID {
			e.checkpoints = append(e.checkpoints, cp)
		}
	}
	src.mu.Unlock()
	p.envs[spec.Name] = e
	return e, nil
}

func TestEnvironmentAction_Fork_HappyPath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024},
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create source: code = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=fork", ForkEnvironmentRequest{Comment: "clone"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork: code = %d, body = %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode environment: %v", err)
	}
	if env.ID == "" || env.ID == "fuse-task-1" {
		t.Errorf("fork id = %q, want a new distinct id", env.ID)
	}
	if env.State != "running" {
		t.Errorf("fork state = %q, want running", env.State)
	}
}

func TestEnvironmentAction_Fork_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-missing?action=fork", ForkEnvironmentRequest{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
}

// ── Host registration ─────────────────────────────────────────────

// newTestHandlerWithProvider is like newTestHandler but also wires
// NewProvider so registerHost (which requires a provider factory) is
// reachable.
func newTestHandlerWithProvider(t *testing.T) (*Handler, *orchestrator.FleetManager) {
	t.Helper()
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	h := &Handler{
		Fleet: fm,
		NewProvider: func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider {
			return newFakeProvider()
		},
	}
	return h, fm
}

func TestRegisterHost_GPUAndBackendRoundTrip(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-1",
		URL:     "http://host-1.test",
		Backend: "qemu",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      2,
			GPUKind:   "a100",
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}

	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Backend != "qemu" {
		t.Errorf("backend = %q, want qemu", info.Backend)
	}
	if info.Capacity.GPUs != 2 {
		t.Errorf("capacity.gpus = %d, want 2", info.Capacity.GPUs)
	}
	if info.Capacity.GPUKind != "a100" {
		t.Errorf("capacity.gpu_kind = %q, want a100", info.Capacity.GPUKind)
	}
}

func TestRegisterHost_AbsentFieldsDefaultToFirecrackerAndZeroGPUs(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-2",
		URL: "http://host-2.test",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}

	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Backend != "firecracker" {
		t.Errorf("backend = %q, want firecracker (default)", info.Backend)
	}
	if info.Capacity.GPUs != 0 || info.Capacity.GPUKind != "" {
		t.Errorf("capacity gpu fields = %+v, want zero values", info.Capacity)
	}
}

func TestRegisterHost_GPUsWithoutQEMUBackendReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-3",
		URL:     "http://host-3.test",
		Backend: "firecracker",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      1,
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeInvalidArgument {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInvalidArgument)
	}
}

func TestRegisterHost_GPUsWithDefaultBackendReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	// Backend omitted (defaults to firecracker) but gpus > 0 requested.
	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-4",
		URL: "http://host-4.test",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      1,
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterHost_ProviderFactoryReceivesBackend(t *testing.T) {
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	var gotBackend orchestrator.HostBackend
	h := &Handler{
		Fleet: fm,
		NewProvider: func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider {
			gotBackend = backend
			return newFakeProvider()
		},
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-qemu",
		URL:     "http://host-qemu.test",
		Backend: "qemu",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      1,
			GPUKind:   "a100",
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	if gotBackend != orchestrator.BackendQEMU {
		t.Errorf("factory backend = %q, want qemu", gotBackend)
	}
}

func TestRegisterHost_RejectsUnknownBackend(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-invalid",
		URL:     "http://host-invalid.test",
		Backend: "unknown",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

// ── Environment create: GPU fields ─────────────────────────────────

func TestCreateEnvironment_GPUFieldsRoundTrip(t *testing.T) {
	h, fm, provider := newTestHandler(t)
	if err := fm.RegisterHost(context.Background(), orchestrator.Host{
		ID:      "gpu-host",
		Backend: orchestrator.BackendQEMU,
		Capacity: orchestrator.HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10, GPUs: 1, GPUKind: "a100",
		},
	}, provider); err != nil {
		t.Fatal(err)
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-gpu",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024, GPUs: 1, GPUKind: "a100"},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Spec.GPUs != 1 {
		t.Errorf("spec.gpus = %d, want 1", env.Spec.GPUs)
	}
	if env.Spec.GPUKind != "a100" {
		t.Errorf("spec.gpu_kind = %q, want a100", env.Spec.GPUKind)
	}
}

func TestCreateEnvironment_GPUWithoutHostReturnsUnavailable(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-gpu",
		Spec:           ResourceSpec{GPUs: 1},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_GPUFieldsAbsentDefaultToZero(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-no-gpu",
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Spec.GPUs != 0 || env.Spec.GPUKind != "" {
		t.Errorf("spec gpu fields = %+v, want zero values", env.Spec)
	}
}
