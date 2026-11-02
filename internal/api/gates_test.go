package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

func gatedEnv(t *testing.T, h *Handler) (http.Handler, string) {
	t.Helper()
	r := mustRouter(t, h)
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-gates",
		ManifestInline: encodeManifest(t),
		Hitl: &HitlSpec{TimeoutSeconds: 300, Gates: []HitlGate{
			{Name: "force-push", On: "exec", Match: HitlGateMatch{Program: []string{"git"}, Args: []string{"push", "--force"}}},
			{Name: "no-destroy", On: "lifecycle", Match: HitlGateMatch{Action: []string{"destroy"}}, Mode: "deny", Reason: "keep it"},
		}},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	return r, "fuse-task-gates"
}

func TestExecGate_heldApprovedWithEdit(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r, envID := gatedEnv(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+envID+"?action=exec",
		ExecEnvironmentRequest{Cmd: []string{"sudo", "git", "push", "--force"}})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("exec: %d %s", rr.Code, rr.Body.String())
	}
	var held HeldResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &held); err != nil {
		t.Fatal(err)
	}
	if held.InterventionID == "" || held.Intervention.Gate != "force-push" || held.Intervention.Kind != "exec" ||
		held.Intervention.Mode != "ask" || !slices.Equal(held.Intervention.Payload.Exec.Argv, []string{"sudo", "git", "push", "--force"}) {
		t.Fatalf("held = %+v", held)
	}
	if loc := rr.Header().Get("Location"); loc != "/v1/interventions/"+held.InterventionID {
		t.Fatalf("Location = %q", loc)
	}

	edited := []string{"git", "push", "--force-with-lease"}
	rr = doJSON(t, r, http.MethodPost, "/v1/interventions/"+held.InterventionID+"?action=approve",
		DecideInterventionRequest{EditedArgv: edited, Grant: "env"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("edit plus grant: %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, r, http.MethodPost, "/v1/interventions/"+held.InterventionID+"?action=approve",
		DecideInterventionRequest{EditedArgv: edited})
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, r, http.MethodGet, "/v1/interventions/"+held.InterventionID+"?wait=5s", nil)
	in := decodeIntervention(t, rr)
	if in.State != "approved" || !slices.Equal(in.Decision.EditedArgv, edited) || in.Decision.Grant != "once" {
		t.Fatalf("decided = %+v", in.Decision)
	}
	if in.Result == nil || (in.Result.Exec == nil && in.Result.Error == "") {
		t.Fatalf("wait returned before the run settled: %+v", in.Result)
	}
}

func TestLifecycleGate_denyReturnsReason(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r, envID := gatedEnv(t, h)

	rr := doJSON(t, r, http.MethodDelete, "/v1/environments/"+envID, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("destroy: %d %s", rr.Code, rr.Body.String())
	}
	var e Error
	_ = json.Unmarshal(rr.Body.Bytes(), &e)
	if e.Error.Message != "keep it" || e.Error.Details["reason"] != "hitl_denied" || e.Error.Details["gate"] != "no-destroy" || e.Error.Details["intervention_id"] == "" {
		t.Fatalf("deny body = %+v", e.Error)
	}
	if rr := doJSON(t, r, http.MethodGet, "/v1/environments/"+envID, nil); rr.Code != http.StatusOK {
		t.Fatalf("env should survive: %d", rr.Code)
	}
	// an ungated action is untouched.
	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+envID+"?action=exec", ExecEnvironmentRequest{Cmd: []string{"ls"}}); rr.Code == http.StatusAccepted {
		t.Fatalf("ls should not be held: %s", rr.Body.String())
	}
}

func TestCreateEnvironment_rejectsInvalidGate(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-bad-gate",
		ManifestInline: encodeManifest(t),
		Hitl:           &HitlSpec{Gates: []HitlGate{{Name: "x", On: "exec"}}},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
}
