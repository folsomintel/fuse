package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

var approver = hitl.Actor{ID: "alice", Source: hitl.ActorKey, KeyID: "key_alice"}

func gateSpec() *hitl.Spec {
	return &hitl.Spec{TimeoutSeconds: 600, Gates: []hitl.Gate{
		{Name: "force-push", On: hitl.KindExec, Match: hitl.GateMatch{Program: []string{"git"}, Args: []string{"push", "--force"}}},
		{Name: "watch-shell", On: hitl.KindExec, Match: hitl.GateMatch{Shell: true}, Mode: hitl.ModeNotify},
		{Name: "no-rm", On: hitl.KindExec, Match: hitl.GateMatch{Program: []string{"rm"}}, Mode: hitl.ModeDeny},
		{Name: "no-destroy", On: hitl.KindLifecycle, Match: hitl.GateMatch{Action: []string{"destroy"}}, Mode: hitl.ModeDeny, Reason: "this env is not yours to destroy"},
		{Name: "snap", On: hitl.KindLifecycle, Match: hitl.GateMatch{Action: []string{"drain"}}},
	}}
}

func gateExec(fm *FleetManager, vmID string, argv ...string) (*hitl.Intervention, error) {
	return fm.GateExec(context.Background(), vmID, hitl.ExecPayload{Argv: argv}, GateCaller{By: testActor})
}

func waitSettled(t *testing.T, fm *FleetManager, id string) hitl.Intervention {
	t.Helper()
	in, err := fm.WaitIntervention(context.Background(), id, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Settled() {
		t.Fatalf("intervention %s not settled: %+v", id, in)
	}
	return in
}

func mockEnvOf(fm *FleetManager, vmID string) *mockEnv {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.vms[vmID].env.(*mockEnv)
}

func TestGateExecHeldApprovedWithEditRunsEdit(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	env := mockEnvOf(fm, vmID)
	env.execResult = ExecResult{ExitCode: 0, Stdout: []byte("pushed")}

	for _, argv := range [][]string{
		{"git", "push", "--force"},
		{"env", "GIT_TRACE=1", "git", "push", "--force"},
		{"sudo", "git", "push", "--force"},
	} {
		held, err := gateExec(fm, vmID, argv...)
		if err != nil || held == nil {
			t.Fatalf("%q should be held: held=%v err=%v", argv, held, err)
		}
		if held.Gate != "force-push" || held.Kind != hitl.KindExec || held.State != hitl.StatePending {
			t.Fatalf("held = %+v", held)
		}
	}

	held, _ := gateExec(fm, vmID, "git", "push", "--force", "origin", "main")
	edited := []string{"git", "push", "--force-with-lease", "origin", "main"}
	in, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{
		Outcome: hitl.OutcomeApprove, EditedArgv: edited, By: approver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Decision.Grant != "once" || !slices.Equal(in.Decision.EditedArgv, edited) {
		t.Fatalf("decision = %+v", in.Decision)
	}
	got := waitSettled(t, fm, held.ID)
	if got.Result == nil || got.Result.Exec == nil || got.Result.Exec.Stdout != "pushed" {
		t.Fatalf("result = %+v", got.Result)
	}
	env.mu.Lock()
	ran := env.execArgv
	env.mu.Unlock()
	if len(ran) != 1 || !slices.Equal(ran[0], edited) {
		t.Fatalf("guest ran %q, want only the edited argv", ran)
	}
}

func TestGateExecEditMatchingDenyIsRefused(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	held, _ := gateExec(fm, vmID, "git", "push", "--force")
	_, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{
		Outcome: hitl.OutcomeApprove, EditedArgv: []string{"sudo", "rm", "-rf", "/"}, By: approver,
	})
	var denied *GateDeniedError
	if !errors.As(err, &denied) || denied.Gate != "no-rm" {
		t.Fatalf("err = %v, want a deny from no-rm", err)
	}
	if in, _ := fm.GetIntervention(context.Background(), held.ID); in.State != hitl.StatePending {
		t.Fatalf("a refused edit must leave the intervention pending, got %s", in.State)
	}
	_, err = fm.DecideIntervention(context.Background(), held.ID, DecideRequest{
		Outcome: hitl.OutcomeApprove, EditedArgv: []string{"git", "push"}, Grant: "env", By: approver,
	})
	if !errors.Is(err, ErrInvalidIntervention) {
		t.Fatalf("edit plus grant: err = %v, want ErrInvalidIntervention", err)
	}
}

func TestGateExecNotifyLetsThroughAndRecords(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	held, err := fm.GateExec(context.Background(), vmID, hitl.ExecPayload{Argv: []string{"sh", "-lc", "ls"}, Shell: "ls"}, GateCaller{By: testActor})
	if err != nil || held != nil {
		t.Fatalf("notify should let it through: held=%v err=%v", held, err)
	}
	list, _, err := fm.ListInterventions(context.Background(), InterventionListFilter{EnvID: vmID})
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	in := list[0]
	if in.Mode != hitl.ModeNotify || in.State != hitl.StateApproved || !in.Settled() || in.Exec.Shell != "ls" {
		t.Fatalf("notify record = %+v", in)
	}
	rows, _, _ := fm.ListHitlAudit(context.Background(), HitlAuditListFilter{})
	var events []hitl.AuditEvent
	for _, r := range rows {
		events = append(events, r.Event)
	}
	if !slices.Equal(events, []hitl.AuditEvent{hitl.AuditRaised, hitl.AuditDecided}) {
		t.Fatalf("audit = %v", events)
	}
}

func TestGateLifecycleDenyKeepsEnv(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	_, err := fm.GateLifecycle(context.Background(), vmID, hitl.ActionDestroy, nil, GateCaller{By: testActor})
	var denied *GateDeniedError
	if !errors.As(err, &denied) || denied.Reason != "this env is not yours to destroy" || denied.InterventionID == "" {
		t.Fatalf("err = %v", err)
	}
	in, err := fm.GetIntervention(context.Background(), denied.InterventionID)
	if err != nil || in.State != hitl.StateRejected || in.Lifecycle.Action != hitl.ActionDestroy {
		t.Fatalf("deny record = %+v, %v", in, err)
	}
	if _, ok := fm.GetVM(vmID); !ok {
		t.Fatal("the env must survive a denied destroy")
	}
	if held, err := fm.GateLifecycle(context.Background(), vmID, hitl.ActionSnapshot, nil, GateCaller{By: testActor}); held != nil || err != nil {
		t.Fatalf("ungated snapshot: held=%v err=%v", held, err)
	}
}

func TestGateLifecycleApprovedRuns(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	held, err := fm.GateLifecycle(context.Background(), vmID, hitl.ActionDrain, nil, GateCaller{By: testActor})
	if err != nil || held == nil {
		t.Fatalf("drain should be held: %v", err)
	}
	if _, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, By: approver}); err != nil {
		t.Fatal(err)
	}
	got := waitSettled(t, fm, held.ID)
	if got.Result == nil || got.Result.Error != "" {
		t.Fatalf("drain result = %+v", got.Result)
	}
	if info, _ := fm.GetVM(vmID); info.State != VMStateDraining {
		t.Fatalf("vm state = %s, want draining", info.State)
	}
}

func TestGateRejectNeverRuns(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	held, _ := gateExec(fm, vmID, "git", "push", "--force")
	if _, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{Outcome: hitl.OutcomeReject, Reason: "no", By: approver}); err != nil {
		t.Fatal(err)
	}
	got := waitSettled(t, fm, held.ID)
	if got.Result != nil || got.Decision.Reason != "no" {
		t.Fatalf("rejected = %+v", got)
	}
	if n := len(mockEnvOf(fm, vmID).execArgv); n != 0 {
		t.Fatalf("a rejected exec ran %d times", n)
	}
}

func TestGateGrantEnvSuppressesRepeat(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	held, _ := gateExec(fm, vmID, "git", "push", "--force")
	if _, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Grant: "env", By: approver}); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, fm, held.ID)
	if again, err := gateExec(fm, vmID, "git", "push", "--force"); again != nil || err != nil {
		t.Fatalf("the grant should cover the identical exec: held=%v err=%v", again, err)
	}
	if other, _ := gateExec(fm, vmID, "git", "push", "--force", "origin"); other == nil {
		t.Fatal("a different argv must still prompt")
	}
	rows, _, _ := fm.ListHitlAudit(context.Background(), HitlAuditListFilter{})
	var created, used bool
	for _, r := range rows {
		created = created || r.Event == hitl.AuditGrantCreated
		used = used || r.Event == hitl.AuditGrantUsed
	}
	if !created || !used {
		t.Fatalf("grant audit rows missing: created=%v used=%v", created, used)
	}
	fm.dropEnvGrants(context.Background(), vmID)
	if again, _ := gateExec(fm, vmID, "git", "push", "--force"); again == nil {
		t.Fatal("dropped grants must not cover anything")
	}
}

func TestGateAllowlistHoldsInterpreter(t *testing.T) {
	spec := &hitl.Spec{Exec: &hitl.ExecPolicy{Default: hitl.ModeAsk, Allow: []string{"git", "ls"}}}
	fm, vmID := hitlFleet(t, FleetConfig{}, spec)
	if held, _ := gateExec(fm, vmID, "ls", "-la"); held != nil {
		t.Fatal("ls is allowed")
	}
	held, _ := gateExec(fm, vmID, "python", "-c", "print(1)")
	if held == nil || held.Gate != hitl.AllowlistGate {
		t.Fatalf("python -c should be held by the allowlist: %+v", held)
	}
}

func TestGateNoSpecIsOff(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, nil)
	if held, err := gateExec(fm, vmID, "git", "push", "--force"); held != nil || err != nil {
		t.Fatalf("no hitl block: held=%v err=%v", held, err)
	}
}

func TestRecoverOrphanRuns(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpec())
	now := time.Now()
	in := hitl.Intervention{
		ID: hitl.NewID(now), EnvID: vmID, Kind: hitl.KindExec, Gate: "force-push", Mode: hitl.ModeAsk,
		State: hitl.StateApproved, Exec: &hitl.ExecPayload{Argv: []string{"git"}}, TriggeredBy: testActor,
		Deadline: now.Add(time.Minute), Decision: &hitl.Decision{Outcome: hitl.OutcomeApprove, DecidedBy: approver, DecidedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	rec, err := fm.interventionToRecord(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.store.UpsertIntervention(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	fm.recoverOrphanRuns(context.Background())
	got, _ := fm.GetIntervention(context.Background(), in.ID)
	if !got.Settled() || got.Result.Error == "" {
		t.Fatalf("orphan not settled: %+v", got.Result)
	}
}
