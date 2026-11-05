package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// guestEnv is a mockEnv that keeps what was uploaded and can attach.
type guestEnv struct {
	*mockEnv
	mu      sync.Mutex
	uploads map[string][]byte
	guest   net.Conn // the guest side of the last attach
}

func (e *guestEnv) Upload(_ context.Context, data []byte, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.uploads == nil {
		e.uploads = make(map[string][]byte)
	}
	e.uploads[path] = bytes.Clone(data)
	return nil
}

func (e *guestEnv) Attach(context.Context, AttachSpec) (io.ReadWriteCloser, error) {
	client, guest := net.Pipe()
	e.mu.Lock()
	e.guest = guest
	e.mu.Unlock()
	return client, nil
}

var (
	humanActor = hitl.Actor{ID: "alice", Source: hitl.ActorKey, KeyID: "key-human"}
	agentActor = hitl.Actor{ID: "key:agent", Source: hitl.ActorKey, KeyID: "key-agent"}
)

func secretSpec() *hitl.Spec {
	return &hitl.Spec{TimeoutSeconds: 600, Requests: hitl.Requests{
		Secret: &hitl.SecretRequests{Targets: []hitl.SecretTarget{
			{Env: "GH_TOKEN", Label: "GitHub token for the deploy bot"},
			{File: "/root/.netrc", Label: "netrc for the registry"},
		}},
		Takeover: &hitl.TakeoverRequests{},
	}}
}

func secretFleet(t *testing.T) (*FleetManager, string, *guestEnv) {
	t.Helper()
	fm, vmID := hitlFleet(t, FleetConfig{TokenEncryptionKey: testEncryptionKey()}, secretSpec())
	env := &guestEnv{mockEnv: &mockEnv{name: vmID}}
	fm.mu.Lock()
	fm.vms[vmID].env = env
	fm.mu.Unlock()
	return fm, vmID, env
}

func requestSecret(fm *FleetManager, vmID, target string) (hitl.Intervention, error) {
	return fm.RequestHuman(context.Background(), vmID, HumanRequest{
		Kind:   hitl.KindSecret,
		Secret: hitl.SecretPayload{Target: target, Message: "please paste the token"},
		By:     agentActor,
	})
}

func TestSecretRequestRejectsUndeclaredTarget(t *testing.T) {
	fm, vmID, _ := secretFleet(t)
	if _, err := requestSecret(fm, vmID, "AWS_SECRET_ACCESS_KEY"); !errors.Is(err, ErrHitlNotDeclared) {
		t.Fatalf("undeclared target: err = %v, want ErrHitlNotDeclared", err)
	}
}

func TestSecretRequestNeedsEncryptionKey(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, secretSpec())
	if _, err := requestSecret(fm, vmID, "GH_TOKEN"); !errors.Is(err, ErrHitlNeedsKey) {
		t.Fatalf("no key: err = %v, want ErrHitlNeedsKey", err)
	}
	if _, _, err := fm.hitlKeysFor(secretSpec(), nil); !errors.Is(err, ErrHitlNeedsKey) {
		t.Fatalf("create without key: err = %v, want ErrHitlNeedsKey", err)
	}
}

func TestSecretLandsInGuestAndNowhereElse(t *testing.T) {
	fm, vmID, env := secretFleet(t)
	ctx := context.Background()
	const value = "ghp_supersecretvalue123"

	in, err := requestSecret(fm, vmID, "GH_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if in.Secret.Label != "GitHub token for the deploy bot" {
		t.Fatalf("label = %q, want the fusefile label", in.Secret.Label)
	}

	// the agent can never answer its own request.
	if _, err := fm.AnswerSecret(ctx, in.ID, SecretAnswerRequest{Value: value, By: agentActor}); !errors.Is(err, hitl.ErrSelfApproval) {
		t.Fatalf("self answer: err = %v, want ErrSelfApproval", err)
	}

	out, err := fm.AnswerSecret(ctx, in.ID, SecretAnswerRequest{Value: value, By: humanActor})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != hitl.StateApproved || out.Decision.Answer != hitl.SecretAnswer {
		t.Fatalf("answered = %+v, want approved with answer %q", out, hitl.SecretAnswer)
	}
	if got := string(env.uploads[hitlEnvDir+"/GH_TOKEN"]); got != value {
		t.Fatalf("guest env file = %q, want the value", got)
	}

	// the value is nowhere in the stored row, the audit log or the events.
	rec, err := fm.store.GetIntervention(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rec)
	if bytes.Contains(raw, []byte(value)) {
		t.Fatal("secret value found in the intervention row")
	}
	audit, _ := fm.store.ListHitlAudit(ctx, AuditFilter{})
	raw, _ = json.Marshal(audit)
	if bytes.Contains(raw, []byte(value)) {
		t.Fatal("secret value found in the audit log")
	}
	if audit[len(audit)-1].Event != hitl.AuditSecretAnswered || audit[len(audit)-1].Actor.ID != "alice" {
		t.Fatalf("last audit row = %+v, want secret_answered by alice", audit[len(audit)-1])
	}
	mem := fm.store.(*MemoryStateStore)
	mem.mu.RLock()
	raw, _ = json.Marshal(mem.events)
	mem.mu.RUnlock()
	if bytes.Contains(raw, []byte(value)) {
		t.Fatal("secret value found in the event log")
	}

	// a second answer is a conflict, not a second write.
	if _, err := fm.AnswerSecret(ctx, in.ID, SecretAnswerRequest{Value: "other", By: humanActor}); !errors.Is(err, ErrInterventionResolved) {
		t.Fatalf("second answer: err = %v, want ErrInterventionResolved", err)
	}
}

func TestSecretFileTargetIsPreparedRootOnly(t *testing.T) {
	fm, vmID, env := secretFleet(t)
	in, err := requestSecret(fm, vmID, "/root/.netrc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fm.AnswerSecret(context.Background(), in.ID, SecretAnswerRequest{Value: "machine x", By: humanActor}); err != nil {
		t.Fatal(err)
	}
	if string(env.uploads["/root/.netrc"]) != "machine x" {
		t.Fatalf("uploads = %v", env.uploads)
	}
	argv := env.execArgv[len(env.execArgv)-1]
	if argv[2] != secretFilePrep || argv[len(argv)-1] != "/root/.netrc" {
		t.Fatalf("prep exec = %q, want the 0600 prep for the file", argv)
	}
	for _, a := range argv {
		if strings.Contains(a, "machine x") {
			t.Fatal("secret value passed on an exec command line")
		}
	}
}

func TestExecSeesSecretEnv(t *testing.T) {
	fm, vmID, env := secretFleet(t)
	if _, err := fm.Exec(context.Background(), vmID, []string{"git", "push"}, ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	argv := env.execArgv[len(env.execArgv)-1]
	want := []string{"sh", "-c", hitlEnvWrapper, "fuse-hitl-env", "git", "push"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("exec argv = %q, want %q", argv, want)
	}
}

func TestTakeoverLocksAgentUntilRelease(t *testing.T) {
	fm, vmID, _ := secretFleet(t)
	ctx := context.Background()

	req, err := fm.RequestHuman(ctx, vmID, HumanRequest{
		Kind: hitl.KindTakeover, Takeover: hitl.TakeoverPayload{Reason: "captcha"}, By: agentActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	// asking is not holding: the agent still runs until a human takes over.
	if _, err := fm.Exec(ctx, vmID, []string{"true"}, ExecOptions{}); err != nil {
		t.Fatalf("exec before takeover: %v", err)
	}
	// the agent may not claim its own takeover request.
	if _, err := fm.StartTakeover(ctx, vmID, TakeoverRequest{By: agentActor}); !errors.Is(err, hitl.ErrSelfApproval) {
		t.Fatalf("self takeover: err = %v, want ErrSelfApproval", err)
	}

	held, err := fm.StartTakeover(ctx, vmID, TakeoverRequest{By: humanActor})
	if err != nil {
		t.Fatal(err)
	}
	if held.ID != req.ID || !held.Locks() {
		t.Fatalf("takeover = %+v, want the agent's request, held", held)
	}
	_, err = fm.Exec(ctx, vmID, []string{"true"}, ExecOptions{})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.InterventionID != req.ID {
		t.Fatalf("exec during takeover: err = %v, want LockedError for %s", err, req.ID)
	}
	if _, err := fm.ComputerAction(ctx, vmID, []byte(`{"action":"screenshot"}`)); !errors.Is(err, ErrEnvLocked) {
		t.Fatalf("computer during takeover: err = %v, want ErrEnvLocked", err)
	}
	other := hitl.Actor{ID: "bob", Source: hitl.ActorKey, KeyID: "key-bob"}
	if _, err := fm.StartTakeover(ctx, vmID, TakeoverRequest{By: other}); !errors.Is(err, ErrTakeoverHeld) {
		t.Fatalf("second takeover: err = %v, want ErrTakeoverHeld", err)
	}

	// a restart forgets the lock in memory; the pending row brings it back.
	fm.hitlMu.Lock()
	fm.hitlLocks = nil
	fm.hitlMu.Unlock()
	fm.syncLocks(ctx)
	if _, err := fm.Exec(ctx, vmID, []string{"true"}, ExecOptions{}); !errors.Is(err, ErrEnvLocked) {
		t.Fatalf("exec after lock resync: err = %v, want ErrEnvLocked", err)
	}

	done := make(chan hitl.Intervention, 1)
	go func() {
		got, _ := fm.WaitIntervention(ctx, req.ID, 10*time.Second)
		done <- got
	}()
	out, err := fm.ReleaseTakeover(ctx, vmID, "solved the captcha", humanActor, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.State != hitl.StateApproved || got.Decision.Answer != "solved the captcha" {
		t.Fatalf("agent got %+v, want the note", got)
	}
	if out.State != hitl.StateApproved {
		t.Fatalf("released = %+v", out)
	}
	if _, err := fm.Exec(ctx, vmID, []string{"true"}, ExecOptions{}); err != nil {
		t.Fatalf("exec after release: %v", err)
	}
	if _, err := fm.ReleaseTakeover(ctx, vmID, "", humanActor, ""); !errors.Is(err, ErrNoTakeover) {
		t.Fatalf("second release: err = %v, want ErrNoTakeover", err)
	}
}

func TestTakeoverLockEndsAtDeadline(t *testing.T) {
	fm, vmID, _ := secretFleet(t)
	ctx := context.Background()
	in, err := fm.StartTakeover(ctx, vmID, TakeoverRequest{By: humanActor, Reason: "poke around"})
	if err != nil {
		t.Fatal(err)
	}
	// age the takeover past its deadline.
	rec, _ := fm.store.GetIntervention(ctx, in.ID)
	rec.Deadline = time.Now().Add(-time.Second)
	_ = fm.store.UpsertIntervention(ctx, rec)
	fm.hitlMu.Lock()
	l := fm.hitlLocks[vmID]
	l.Deadline = rec.Deadline
	fm.hitlLocks[vmID] = l
	fm.hitlMu.Unlock()

	if _, err := fm.Exec(ctx, vmID, []string{"true"}, ExecOptions{}); err != nil {
		t.Fatalf("exec after deadline: %v", err)
	}
	fm.reconcileInterventions(ctx)
	got, _ := fm.GetIntervention(ctx, in.ID)
	if got.State != hitl.StateExpired {
		t.Fatalf("state = %s, want expired", got.State)
	}
	fm.hitlMu.Lock()
	_, still := fm.hitlLocks[vmID]
	fm.hitlMu.Unlock()
	if still {
		t.Fatal("lock survived its expired intervention")
	}
}

// readFrame reads one fuse-attach/1 frame.
func readFrame(t *testing.T, r *bufio.Reader) (byte, []byte) {
	t.Helper()
	var h [attachFrameHeader]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, binary.BigEndian.Uint32(h[4:8]))
	if _, err := io.ReadFull(r, p); err != nil {
		t.Fatal(err)
	}
	return h[0], p
}

func TestTakeoverAttachIsRecordedAndSealed(t *testing.T) {
	fm, vmID, env := secretFleet(t)
	ctx := context.Background()
	in, err := fm.StartTakeover(ctx, vmID, TakeoverRequest{By: humanActor})
	if err != nil {
		t.Fatal(err)
	}
	other := hitl.Actor{ID: "bob", Source: hitl.ActorKey, KeyID: "key-bob"}
	if _, err := fm.TakeoverAttach(ctx, in.ID, AttachSpec{TTY: true}, other, false, ""); !errors.Is(err, ErrNotTakeoverHolder) {
		t.Fatalf("attach by non-holder: err = %v, want ErrNotTakeoverHolder", err)
	}

	stream, err := fm.TakeoverAttach(ctx, in.ID, AttachSpec{TTY: true, Rows: 30, Cols: 100}, humanActor, false, "")
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(stream)
	typ, banner := readFrame(t, r)
	if typ != attachFrameStdout || !strings.Contains(string(banner), "this session is recorded") {
		t.Fatalf("first frame = %d %q, want the banner", typ, banner)
	}

	go func() {
		_, _ = env.guest.Write(encodeAttachFrame(attachFrameStdout, []byte("root@guest:~# ")))
	}()
	if _, p := readFrame(t, r); string(p) != "root@guest:~# " {
		t.Fatalf("relayed = %q", p)
	}
	// keystrokes are relayed but never recorded.
	go func() { _, _ = io.Copy(io.Discard, env.guest) }()
	if _, err := stream.Write(encodeAttachFrame(0, []byte("hunter2\n"))); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	audit, _ := fm.store.ListHitlAudit(ctx, AuditFilter{})
	last := audit[len(audit)-1]
	if last.Event != hitl.AuditTakeoverSession || last.RecordingID == "" {
		t.Fatalf("last audit row = %+v, want takeover_session with a recording", last)
	}
	if err := hitl.VerifyChain(audit); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	stored, err := fm.store.GetHitlRecording(ctx, last.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Sealed || bytes.Contains(stored.Data, []byte("root@guest")) {
		t.Fatal("recording is stored in the clear")
	}
	rec, err := fm.GetRecording(ctx, last.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(rec.Cast)), "\n")
	var header map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header["version"] != float64(2) || header["width"] != float64(100) {
		t.Fatalf("cast header = %q", lines[0])
	}
	if !strings.Contains(string(rec.Cast), "root@guest") || strings.Contains(string(rec.Cast), "hunter2") {
		t.Fatalf("cast = %s, want output and no keystrokes", rec.Cast)
	}

	// without the key the cast does not open.
	fm.tokenEncryptionKey = nil
	if _, err := fm.GetRecording(ctx, last.RecordingID); err == nil {
		t.Fatal("recording decrypted without the orchestrator key")
	}
}
