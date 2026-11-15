package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

func buildBody() CreateBuildRequest {
	return CreateBuildRequest{
		Name: "app",
		Spec: ResourceSpec{CPUs: 1, RamMB: 256},
		Setup: BuildSetup{
			Prelude: "set -eu\n",
			Steps:   []BuildStep{{Script: "true", Key: "k0"}, {Script: "date"}},
		},
	}
}

func postBuild(t *testing.T, h http.Handler, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/builds", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(HeaderIdempotencyKey, key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeBuild(t *testing.T, rr *httptest.ResponseRecorder) Build {
	t.Helper()
	var b Build
	if err := json.NewDecoder(rr.Body).Decode(&b); err != nil {
		t.Fatalf("decode build: %v", err)
	}
	return b
}

func TestCreateBuildAnswers202AndFinishes(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := mustRouter(t, h)

	rr := postBuild(t, router, "", buildBody())
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	b := decodeBuild(t, rr)
	if b.State != orchestrator.BuildRunning || rr.Header().Get("Location") != "/v1/builds/"+b.ID {
		t.Fatalf("build %+v location %q", b, rr.Header().Get("Location"))
	}
	if len(b.Layers) != 2 || b.Layers[0].Status != "miss" || b.Layers[1].Status != "skip" {
		t.Errorf("layers %+v, want miss then skip", b.Layers)
	}

	got := doJSON(t, router, http.MethodGet, "/v1/builds/"+b.ID+"?wait=10s", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("get: %d %s", got.Code, got.Body)
	}
	if done := decodeBuild(t, got); done.State == orchestrator.BuildRunning {
		t.Fatalf("build still running after the wait: %+v", done)
	}
}

func TestCreateBuildIdempotencyKey(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := mustRouter(t, h)

	first := postBuild(t, router, "k-1", buildBody())
	if first.Code != http.StatusAccepted {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	id := decodeBuild(t, first).ID

	again := postBuild(t, router, "k-1", buildBody())
	if again.Code != http.StatusAccepted || again.Header().Get(HeaderIdempotentReplayed) != "true" {
		t.Fatalf("replay: %d replayed=%q", again.Code, again.Header().Get(HeaderIdempotentReplayed))
	}
	if got := decodeBuild(t, again).ID; got != id {
		t.Fatalf("replay id %s, want %s", got, id)
	}

	other := buildBody()
	other.Name = "other"
	if rr := postBuild(t, router, "k-1", other); rr.Code != http.StatusConflict {
		t.Fatalf("reuse for another build: %d %s", rr.Code, rr.Body)
	}
}

func TestCreateBuildValidates(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := mustRouter(t, h)
	cases := map[string]func(*CreateBuildRequest){
		"no name":    func(r *CreateBuildRequest) { r.Name = "" },
		"no prelude": func(r *CreateBuildRequest) { r.Setup.Prelude = "" },
		"no steps":   func(r *CreateBuildRequest) { r.Setup.Steps = nil },
		"empty step": func(r *CreateBuildRequest) { r.Setup.Steps[0].Script = "" },
		"gpu":        func(r *CreateBuildRequest) { r.Spec.GPUs = 1 },
		"bad key":    nil,
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := buildBody()
			key := ""
			if mutate == nil {
				key = "has space"
			} else {
				mutate(&body)
			}
			if rr := postBuild(t, router, key, body); rr.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rr.Code, rr.Body)
			}
		})
	}
	body := buildBody()
	body.Spec.HostID = "nope"
	if rr := postBuild(t, router, "", body); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown host: %d", rr.Code)
	}
}

// a build is readable only by the credential scope that started it.
func TestGetBuildIsTenantScoped(t *testing.T) {
	h, fm, _ := newTestHandler(t)
	router := mustRouter(t, h)
	if rr := doJSON(t, router, http.MethodGet, "/v1/builds/bld_missing", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown build: %d", rr.Code)
	}

	rec, _, err := fm.SubmitBuild(context.Background(), orchestrator.BuildInput{
		ID: "bld_tenant", Name: "app", TenantID: "key-a",
		Setup: buildSetupPlan(), Keys: []string{""},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p    Principal
		want int
	}{
		{Principal{KeyID: "key-a"}, http.StatusOK},
		{Principal{KeyID: "key-b"}, http.StatusNotFound},
		{Principal{Master: true}, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/builds/"+rec.ID, nil)
		req = req.WithContext(withPrincipal(req.Context(), tc.p))
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("%+v: status %d, want %d", tc.p, rr.Code, tc.want)
		}
	}
}

func buildSetupPlan() fusefile.SetupPlan {
	return fusefile.SetupPlan{Prelude: "set -eu\n", Steps: []string{"true"}}
}
