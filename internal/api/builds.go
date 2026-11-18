package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/folsomintel/fuse/internal/fusefile"
	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/secrets"
)

// CreateBuildRequest is the body of POST /v1/builds: a Fusefile's setup phase
// as the client planned it. the client derives the layer keys because only it
// can read the files a step's `inputs` name; the orchestrator runs the plan.
type CreateBuildRequest struct {
	// Name labels the build artifact and its layers.
	Name string `json:"name"`
	// Spec sizes the builder. gpu builds are refused: a gpu host cannot
	// snapshot.
	Spec ResourceSpec `json:"spec"`
	// ManifestInline is the builder's base64 manifest, as on create.
	ManifestInline string            `json:"manifest_inline,omitempty"`
	Secrets        map[string]string `json:"secrets,omitempty"`
	// Arch is the arch layers are looked up and filed under. empty derives
	// it from the fleet; a fleet of mixed arches then builds without the
	// cache.
	Arch string `json:"arch,omitempty"`
	// Keep leaves the builder running instead of destroying it.
	Keep  bool       `json:"keep,omitempty"`
	Setup BuildSetup `json:"setup"`
}

// BuildSetup is the setup phase cut into what the builder execs: the prelude
// every exec starts with, the file block written on a cold build's first
// exec, and one fragment per step.
type BuildSetup struct {
	Prelude string      `json:"prelude"`
	Files   string      `json:"files,omitempty"`
	Steps   []BuildStep `json:"steps"`
}

// BuildStep is one setup step. Key is its layer cache key; a step without
// one, and every step after it, is not cached.
type BuildStep struct {
	Script string `json:"script"`
	Key    string `json:"key,omitempty"`
}

// Build is a fusefile build as GET /v1/builds/{id} reports it.
type Build struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// State is running, succeeded or failed.
	State string `json:"state" enums:"running,succeeded,failed"`
	// Step is the step the build is on: queued, resolve_cache,
	// create_builder, layer_<i>, tail, snapshot, destroy_builder or done.
	Step      string `json:"step,omitempty"`
	BuilderID string `json:"builder_id,omitempty"`
	Arch      string `json:"arch,omitempty"`
	// CacheHits is how many setup steps the seed artifact covered.
	CacheHits      int          `json:"cache_hits"`
	SeedSnapshotID string       `json:"seed_snapshot_id,omitempty"`
	Layers         []BuildLayer `json:"layers"`
	// SnapshotID is the build artifact, once taken.
	SnapshotID string `json:"snapshot_id,omitempty"`
	Error      string `json:"error,omitempty"`
	// Output is the redacted tail of the setup exec that failed.
	Output     string     `json:"output,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// BuildLayer is one setup step of a build: hit (the seed covers it), miss
// (it runs and is snapshotted) or skip (it cannot be cached).
type BuildLayer struct {
	Index      int    `json:"index"`
	Key        string `json:"key,omitempty"`
	Status     string `json:"status" enums:"hit,miss,skip"`
	SnapshotID string `json:"snapshot_id,omitempty"`
}

func toAPIBuild(rec orchestrator.BuildRecord) Build {
	layers := make([]BuildLayer, len(rec.Layers))
	for i, l := range rec.Layers {
		layers[i] = BuildLayer{Index: l.Index, Key: l.Key, Status: l.Status, SnapshotID: l.SnapshotID}
	}
	return Build{
		ID: rec.ID, Name: rec.Name, State: rec.State, Step: rec.Step,
		BuilderID: rec.BuilderID, Arch: rec.Arch, CacheHits: rec.CacheHits,
		SeedSnapshotID: rec.SeedSnapshotID, Layers: layers, SnapshotID: rec.SnapshotID,
		Error: rec.Error, Output: rec.Output,
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt, FinishedAt: rec.FinishedAt,
	}
}

// buildID is the build's id: derived from the caller and its Idempotency-Key,
// so a retry names the build the first request started even after a restart,
// or random without a key.
func buildID(owner, key string) string {
	if key == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		return "bld_" + hex.EncodeToString(b[:])
	}
	sum := sha256.Sum256([]byte(owner + "\x00" + key))
	return "bld_" + hex.EncodeToString(sum[:12])
}

// buildFingerprint identifies the request a build id was first used for. the
// record keeps it, so it covers secret names but never their values: a hash
// of a short secret is a guess away from the secret.
func buildFingerprint(req CreateBuildRequest) (string, error) {
	names := make([]string, 0, len(req.Secrets))
	for k := range req.Secrets {
		names = append(names, k)
	}
	sort.Strings(names)
	req.Secrets = nil
	raw, err := json.Marshal(struct {
		CreateBuildRequest
		SecretNames []string `json:"secret_names"`
	}{req, names})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// createBuild submits a build and answers 202 with it at once: a build runs
// for minutes, past any request. it keeps running when the caller goes away.
//
//	@Summary		Create build
//	@Description	Runs a Fusefile's setup phase server-side: boots a builder, runs each setup step (snapshotting each cacheable one as a layer), snapshots the result and destroys the builder. The client plans the layers; the orchestrator resolves the layer cache and runs the rest. Answers 202 with the build at once; follow it with GET /v1/builds/{buildId}. An Idempotency-Key names the build, so a retry returns it instead of starting another.
//	@Tags			builds
//	@Accept			json
//	@Produce		json
//	@Param			body	body		CreateBuildRequest	true	"Build"
//	@Success		202		{object}	Build
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		409		{object}	Error
//	@Failure		503		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/builds [post]
func (h *Handler) createBuild(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(HeaderIdempotencyKey)
	if key != "" && !validIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			HeaderIdempotencyKey+": want 1 to 255 visible ascii characters", nil)
		return
	}
	body, ok := readOperationBody(w, r, MaxEnvironmentBodyBytes)
	if !ok {
		return
	}
	var req CreateBuildRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid json body: "+err.Error(), nil)
		return
	}
	if err := h.validateBuild(req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	if req.Spec.HostID != "" {
		if _, ok := h.Fleet.GetHost(req.Spec.HostID); !ok {
			writeError(w, http.StatusNotFound, CodeNotFound,
				fmt.Sprintf("spec.host_id: host %q is not registered", req.Spec.HostID), nil)
			return
		}
	}
	manifest, err := h.resolver().Resolve(CreateEnvironmentRequest{ManifestInline: req.ManifestInline})
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	if err := secrets.ValidateSecrets(manifest, req.Secrets); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}

	owner := operationOwner(principal(r))
	fingerprint, err := buildFingerprint(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	in := orchestrator.BuildInput{
		ID:          buildID(owner, key),
		Name:        req.Name,
		TenantID:    callerTenantID(r.Context()),
		Fingerprint: fingerprint,
		Spec:        toOrchestratorSpec(req.Spec),
		Manifest:    manifest,
		Arch:        orchestrator.NormalizeArch(req.Arch),
		Keep:        req.Keep,
		Setup:       fusefile.SetupPlan{Prelude: endLine(req.Setup.Prelude), Files: endLine(req.Setup.Files)},
	}
	for _, s := range req.Setup.Steps {
		in.Setup.Steps = append(in.Setup.Steps, s.Script)
		in.Keys = append(in.Keys, s.Key)
	}
	rec, replayed, err := h.Fleet.SubmitBuild(r.Context(), in, req.Secrets)
	if err != nil {
		if errors.Is(err, orchestrator.ErrBuildConflict) {
			writeError(w, http.StatusConflict, CodeConflict,
				HeaderIdempotencyKey+" was already used for a different build",
				map[string]string{"reason": "idempotency_key_reused", "build_id": in.ID})
			return
		}
		writeFleetErrorRedacted(w, err, req.Secrets)
		return
	}
	if replayed {
		w.Header().Set(HeaderIdempotentReplayed, "true")
	}
	w.Header().Set("Location", "/v1/builds/"+rec.ID)
	writeJSON(w, http.StatusAccepted, toAPIBuild(rec))
}

// validateBuild checks what the builder's create would check, before the
// build is recorded.
func (h *Handler) validateBuild(req CreateBuildRequest) error {
	if req.Name == "" {
		return errors.New("name is required")
	}
	if req.Setup.Prelude == "" {
		return errors.New("setup.prelude is required")
	}
	if len(req.Setup.Steps) == 0 {
		return errors.New("setup.steps: a build needs at least one setup step")
	}
	for i, s := range req.Setup.Steps {
		if s.Script == "" {
			return fmt.Errorf("setup.steps[%d].script is required", i)
		}
	}
	if req.Spec.GPUs > 0 || req.Spec.GPUKind != "" || req.Spec.GPUProfile != "" {
		return errors.New("spec: gpu builds are not supported, a gpu host cannot snapshot")
	}
	if err := validatePlacementSpec(req.Spec); err != nil {
		return err
	}
	if err := validateImageDigest(req.Spec, "", false); err != nil {
		return err
	}
	return validateTimeoutSpec(req.Spec)
}

// getBuild reports a build, attaching for up to ?wait= until it finishes.
//
//	@Summary		Get build
//	@Description	Reports a build: its state, the step it is on, each setup layer's cache status and, once finished, the artifact or the error with the failing step's output. ?wait= attaches until the build is no longer running, bounded below the server's write timeout. A build is readable by the credential that started it, and by the master token.
//	@Tags			builds
//	@Produce		json
//	@Param			buildId	path		string	true	"Build identifier"
//	@Param			wait	query		string	false	"How long to wait for the build to finish, e.g. 30s"
//	@Success		200		{object}	Build
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/builds/{buildId} [get]
func (h *Handler) getBuild(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "buildId")
	wait := time.Duration(0)
	if r.URL.Query().Has("wait") {
		var err error
		if wait, err = h.parseWait(r); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
			return
		}
	}
	p := principal(r)
	deadline := time.Now().Add(wait)
	for {
		rec, err := h.Fleet.GetBuild(r.Context(), id)
		if err == nil && !p.Master && rec.TenantID != callerTenantID(r.Context()) {
			err = orchestrator.ErrBuildNotFound
		}
		if err != nil {
			writeFleetError(w, err)
			return
		}
		if rec.State != orchestrator.BuildRunning || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, toAPIBuild(rec))
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(min(buildPollInterval, time.Until(deadline))):
		}
	}
}

// endLine ends a non-empty script fragment with a newline, so the next one
// joined after it starts a command of its own. a fragment a Fusefile compiles
// to already ends with one and is unchanged.
func endLine(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// buildPollInterval is how often a waiting GET re-reads the build.
const buildPollInterval = 500 * time.Millisecond
