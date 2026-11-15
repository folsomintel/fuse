package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/fusefile"
	"github.com/folsomintel/fuse/internal/secrets"
)

// build states.
const (
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
)

// layer statuses, the same words `fuse build --plan` prints.
const (
	// LayerHit is a step the seed artifact already covers. it does not run.
	LayerHit = "hit"
	// LayerMiss is a cacheable step with no stored artifact. it runs and is
	// snapshotted as a layer.
	LayerMiss = "miss"
	// LayerSkip is a step that cannot be cached. it runs in the tail.
	LayerSkip = "skip"
)

// build steps as the record and its events report them. an exec step is
// "layer_<i>" for a cacheable step, or "tail".
const (
	BuildStepQueued    = "queued"
	BuildStepResolve   = "resolve_cache"
	BuildStepCreate    = "create_builder"
	BuildStepTail      = "tail"
	BuildStepSnapshot  = "snapshot"
	BuildStepDestroy   = "destroy_builder"
	BuildStepDone      = "done"
	buildStepLayerName = "layer_%d"
)

// buildExecTimeout bounds one setup exec, the host agent's own ceiling.
const buildExecTimeout = MaxExecTimeout

// buildOutputTail is how much of a failed exec's output the record keeps.
const buildOutputTail = 16 << 10

var (
	// ErrBuildNotFound is a build id with no record.
	ErrBuildNotFound = errors.New("build not found")
	// ErrBuildConflict is a build id (an Idempotency-Key) already used for a
	// different build.
	ErrBuildConflict = errors.New("build id already used for a different build")
	// ErrBuildNotStarted is a build restate did not take, so nothing runs
	// it.
	ErrBuildNotStarted = errors.New("build could not be started")
	// errBuildStepFailed is a setup exec that exited non-zero. a retry runs
	// the same script on the same disk, so it is final.
	errBuildStepFailed = errors.New("build step failed")
	// errBuilderBusy is a builder from an earlier attempt that is not
	// running yet. the create step waits for it.
	errBuilderBusy = errors.New("builder from an earlier attempt is not running yet")
	// errBuildSealed is build secrets that no longer decrypt.
	errBuildSealed = errors.New("build secrets do not decrypt under TOKEN_ENCRYPTION_KEY")
)

// PermanentBuildError reports whether a build step failed in a way a retry
// cannot change: a setup exec that exited non-zero, a fleet with no room for
// the builder, secrets that no longer decrypt, or a refusal a snapshot or
// restore step would also treat as final.
func PermanentBuildError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errBuildStepFailed) || errors.Is(err, ErrNoCapacity) ||
		errors.Is(err, ErrNoHosts) || errors.Is(err, errBuildSealed) ||
		PermanentSnapshotError(err)
}

// BuildLayer is one setup step of a build.
type BuildLayer struct {
	Index      int    `json:"index"`
	Key        string `json:"key,omitempty"`
	Status     string `json:"status"`
	SnapshotID string `json:"snapshot_id,omitempty"`
}

// BuildRecord is a build as the state store keeps it. GET /v1/builds/{id}
// reads it; restate never does.
type BuildRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TenantID string `json:"tenant_id"`
	// Fingerprint identifies the request that started the build, so a reuse
	// of its id for a different request is refused.
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Step        string `json:"step"`
	// Durable is set when restate runs the build. one run in process ends
	// with the process.
	Durable        bool         `json:"durable"`
	BuilderID      string       `json:"builder_id,omitempty"`
	Arch           string       `json:"arch,omitempty"`
	CacheHits      int          `json:"cache_hits"`
	SeedSnapshotID string       `json:"seed_snapshot_id,omitempty"`
	Layers         []BuildLayer `json:"layers"`
	SnapshotID     string       `json:"snapshot_id,omitempty"`
	Error          string       `json:"error,omitempty"`
	// Output is the redacted tail of the setup exec that failed.
	Output     string     `json:"output,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// BuildStore keeps build records. both state stores implement it.
type BuildStore interface {
	// InsertBuild stores rec unless a build with its id exists, atomically.
	// claimed is false when one does, and prev is that build.
	InsertBuild(ctx context.Context, rec BuildRecord) (prev BuildRecord, claimed bool, err error)
	UpsertBuild(ctx context.Context, rec BuildRecord) error
	// GetBuild returns one build, or an error wrapping ErrBuildNotFound.
	GetBuild(ctx context.Context, id string) (BuildRecord, error)
	DeleteBuild(ctx context.Context, id string) error
}

// BuildInput is what a build runs from: the restate workflow's journaled
// input, so its secrets travel sealed under TOKEN_ENCRYPTION_KEY.
type BuildInput struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	TenantID    string             `json:"tenant_id"`
	Fingerprint string             `json:"fingerprint"`
	Spec        Spec               `json:"spec"`
	Manifest    []byte             `json:"manifest"`
	Sealed      []byte             `json:"sealed_secrets,omitempty"`
	Arch        string             `json:"arch,omitempty"`
	Keep        bool               `json:"keep,omitempty"`
	Setup       fusefile.SetupPlan `json:"setup"`
	// Keys is the client's layer key per setup step, "" for a step that
	// cannot be cached. only the leading run of keys counts: the chain
	// breaks for good at the first step without one.
	Keys []string `json:"keys"`

	// secrets is the plaintext of an in-process build. never journaled.
	secrets map[string]string
}

// BuildPlan is what the cache lookup settled: the seed the builder boots from
// and the execs the build runs, in order.
type BuildPlan struct {
	Arch           string      `json:"arch,omitempty"`
	SeedSnapshotID string      `json:"seed_snapshot_id,omitempty"`
	SeedHostID     string      `json:"seed_host_id,omitempty"`
	Execs          []BuildExec `json:"execs"`
}

// BuildExec is one setup exec: steps [From, To) of the setup plan, the file
// block first when Files is set. Layer is the step it snapshots afterwards,
// or -1.
type BuildExec struct {
	Step  string `json:"step"`
	Label string `json:"label"`
	From  int    `json:"from"`
	To    int    `json:"to"`
	Files bool   `json:"files"`
	Layer int    `json:"layer"`
}

// DurableBuilds runs a build as the restate Build/{build_id} workflow.
// *durable.Client satisfies it.
type DurableBuilds interface {
	// SubmitBuild starts the workflow without waiting for it. a second
	// submit of the same id attaches to the first.
	SubmitBuild(ctx context.Context, in BuildInput) error
}

// buildRegistry is the fleet's build bookkeeping.
type buildRegistry struct {
	mu      sync.Mutex
	durable DurableBuilds
	// local counts, per build id, the in-process runs of this process and
	// the submits about to start one. a count, since a submit that loses
	// the claim to a concurrent one must not unmark the winner.
	local map[string]int
}

// SetDurableBuilds runs builds on restate. nil (mode off, the default) runs
// them in this process. set it before Start.
func (fm *FleetManager) SetDurableBuilds(d DurableBuilds) {
	fm.builds.mu.Lock()
	fm.builds.durable = d
	fm.builds.mu.Unlock()
}

func (fm *FleetManager) durableBuilds() DurableBuilds {
	fm.builds.mu.Lock()
	defer fm.builds.mu.Unlock()
	return fm.builds.durable
}

func (fm *FleetManager) buildStore() (BuildStore, error) {
	bs, ok := fm.store.(BuildStore)
	if !ok {
		return nil, errors.New("state store does not keep builds")
	}
	return bs, nil
}

// leadingKeys is how many setup steps can produce a layer: the leading run of
// non-empty keys.
func leadingKeys(keys []string) int {
	n := 0
	for _, k := range keys {
		if k == "" {
			break
		}
		n++
	}
	return n
}

// newBuildRecord is in's record before anything ran: every cacheable step a
// miss, the rest skipped.
func newBuildRecord(in BuildInput, now time.Time) BuildRecord {
	prefix := leadingKeys(in.Keys)
	layers := make([]BuildLayer, len(in.Setup.Steps))
	for i := range layers {
		layers[i] = BuildLayer{Index: i, Status: LayerSkip}
		if i < prefix {
			layers[i].Key = in.Keys[i]
			layers[i].Status = LayerMiss
		}
	}
	return BuildRecord{
		ID:          in.ID,
		Name:        in.Name,
		TenantID:    in.TenantID,
		Fingerprint: in.Fingerprint,
		State:       BuildRunning,
		Step:        BuildStepQueued,
		Arch:        in.Arch,
		Layers:      layers,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// SubmitBuild records a new build and starts it: on restate when a durable
// runner is set, in this process otherwise. a build whose id already has a
// record is returned as it is (replayed=true), or refused with
// ErrBuildConflict when it was started by a different request. secretMap is
// the builder's secrets in plaintext; it is sealed before anything is
// journaled.
func (fm *FleetManager) SubmitBuild(ctx context.Context, in BuildInput, secretMap map[string]string) (rec BuildRecord, replayed bool, err error) {
	bs, err := fm.buildStore()
	if err != nil {
		return BuildRecord{}, false, err
	}
	if len(in.Setup.Steps) == 0 {
		return BuildRecord{}, false, errors.New("build: no setup steps")
	}
	d := fm.durableBuilds()
	if _, memory := bs.(*MemoryStateStore); d != nil && memory {
		// a restart would lose the record while restate resumed the build,
		// and the steps it replays from the journal would not write it
		// again.
		fm.logger.WarnContext(ctx, "builds need a persistent state store to run on restate, running it in process", "build", in.ID)
		d = nil
	}
	if d != nil && len(secretMap) > 0 && len(fm.tokenEncryptionKey) != 32 {
		// the secrets could not be sealed for the journal.
		fm.logger.WarnContext(ctx, "build has secrets and no TOKEN_ENCRYPTION_KEY, running it in process", "build", in.ID)
		d = nil
	}
	if d != nil && len(secretMap) > 0 {
		raw, err := json.Marshal(secretMap)
		if err != nil {
			return BuildRecord{}, false, err
		}
		if in.Sealed, err = secrets.EncryptBytes(raw, fm.tokenEncryptionKey); err != nil {
			return BuildRecord{}, false, fmt.Errorf("seal build secrets: %w", err)
		}
	}

	rec = newBuildRecord(in, time.Now().UTC())
	rec.Durable = d != nil
	if d == nil {
		// marked local before the record exists, so a concurrent read never
		// takes it for a build whose process went away.
		fm.setLocalBuild(in.ID, true)
	}
	// the claim is atomic: of two requests for one id, one inserts the
	// record and runs the build, the other gets that record back.
	prev, claimed, err := bs.InsertBuild(ctx, rec)
	if err != nil || !claimed {
		if d == nil {
			fm.setLocalBuild(in.ID, false)
		}
		if err != nil {
			return BuildRecord{}, false, fmt.Errorf("persist build %s: %w", in.ID, err)
		}
		if prev.Fingerprint != in.Fingerprint {
			return BuildRecord{}, false, fmt.Errorf("%w: %s", ErrBuildConflict, in.ID)
		}
		if prev.Durable && prev.State == BuildRunning && d != nil {
			// the first submit may have stopped between the record and the
			// send. restate runs a workflow id once, so sending it again
			// starts a build nothing started and attaches to one that runs.
			if err := d.SubmitBuild(ctx, in); err != nil {
				return BuildRecord{}, false, fmt.Errorf("%w: resubmit build %s: %v", ErrBuildNotStarted, in.ID, err)
			}
		}
		return fm.liveBuild(ctx, bs, prev), true, nil
	}
	fm.appendEvent(ctx, "build", in.ID, "build.submitted", map[string]any{"name": in.Name, "durable": rec.Durable})

	if d != nil {
		if err := d.SubmitBuild(ctx, in); err != nil {
			// nothing runs it, so the record goes and a retry starts over.
			if derr := bs.DeleteBuild(context.WithoutCancel(ctx), in.ID); derr != nil {
				fm.logger.ErrorContext(ctx, "drop unsubmitted build record failed", "build", in.ID, "err", derr)
			}
			return BuildRecord{}, false, fmt.Errorf("%w: submit build %s: %v", ErrBuildNotStarted, in.ID, err)
		}
		return rec, false, nil
	}

	in.secrets = secretMap
	// a build outlives its request and is not waited for on shutdown: one
	// cut short is failed by the next read of its record.
	go func() {
		defer fm.setLocalBuild(in.ID, false)
		fm.runBuild(context.WithoutCancel(ctx), in)
	}()
	return rec, false, nil
}

// setLocalBuild counts a run of build id in this process in (running) or
// out.
func (fm *FleetManager) setLocalBuild(id string, running bool) {
	fm.builds.mu.Lock()
	defer fm.builds.mu.Unlock()
	if fm.builds.local == nil {
		fm.builds.local = map[string]int{}
	}
	if running {
		fm.builds.local[id]++
		return
	}
	if fm.builds.local[id]--; fm.builds.local[id] <= 0 {
		delete(fm.builds.local, id)
	}
}

// GetBuild returns a build's record.
func (fm *FleetManager) GetBuild(ctx context.Context, id string) (BuildRecord, error) {
	bs, err := fm.buildStore()
	if err != nil {
		return BuildRecord{}, err
	}
	rec, err := bs.GetBuild(ctx, id)
	if err != nil {
		return BuildRecord{}, err
	}
	return fm.liveBuild(ctx, bs, rec), nil
}

// liveBuild settles an in-process build that is no longer running anywhere:
// its process went away before it finished, so it is failed rather than left
// running forever.
func (fm *FleetManager) liveBuild(ctx context.Context, bs BuildStore, rec BuildRecord) BuildRecord {
	if rec.State != BuildRunning || rec.Durable {
		return rec
	}
	fm.builds.mu.Lock()
	local := fm.builds.local[rec.ID] > 0
	fm.builds.mu.Unlock()
	if local {
		return rec
	}
	now := time.Now().UTC()
	rec.State = BuildFailed
	rec.Error = "the orchestrator restarted before the build finished"
	rec.UpdatedAt = now
	rec.FinishedAt = &now
	if err := bs.UpsertBuild(ctx, rec); err != nil {
		fm.logger.WarnContext(ctx, "mark orphaned build failed", "build", rec.ID, "err", err)
	}
	return rec
}

// runBuild runs in's steps in this process, in the order the Build workflow
// runs them.
func (fm *FleetManager) runBuild(ctx context.Context, in BuildInput) {
	ctx, end := fm.startOperation(ctx, "build", in.ID)
	plan, err := fm.BuildResolveCache(ctx, in)
	var vmID string
	if err == nil {
		vmID, err = fm.BuildCreateBuilder(ctx, in, plan)
	}
	for _, ex := range plan.Execs {
		if err != nil {
			break
		}
		if err = fm.BuildExec(ctx, in, vmID, ex); err == nil && ex.Layer >= 0 {
			_, err = fm.BuildSnapshotLayer(ctx, in, vmID, ex.Layer)
		}
	}
	var snapshotID string
	if err == nil {
		snapshotID, err = fm.BuildSnapshot(ctx, in, vmID)
	}
	outcome := BuildSucceeded
	if err != nil {
		outcome = BuildFailed
	}
	if derr := fm.BuildDestroyBuilder(ctx, in, outcome); derr != nil {
		fm.logger.WarnContext(ctx, "destroy builder failed", "build", in.ID, "err", derr)
	}
	if err != nil {
		if ferr := fm.BuildFailed(ctx, in, err.Error()); ferr != nil {
			fm.logger.ErrorContext(ctx, "record failed build", "build", in.ID, "err", ferr)
		}
	} else if ferr := fm.BuildFinish(ctx, in, snapshotID); ferr != nil {
		fm.logger.ErrorContext(ctx, "record finished build", "build", in.ID, "err", ferr)
	}
	end(err)
}

// updateBuild changes in's record under the build lock. a record that is
// gone (a memory store after a restart restate resumed across) starts again
// from in.
func (fm *FleetManager) updateBuild(ctx context.Context, in BuildInput, fn func(*BuildRecord)) (BuildRecord, error) {
	bs, err := fm.buildStore()
	if err != nil {
		return BuildRecord{}, err
	}
	fm.builds.mu.Lock()
	defer fm.builds.mu.Unlock()
	rec, err := bs.GetBuild(ctx, in.ID)
	if errors.Is(err, ErrBuildNotFound) {
		rec, err = newBuildRecord(in, time.Now().UTC()), nil
		rec.Durable = fm.builds.durable != nil
	}
	if err != nil {
		return BuildRecord{}, err
	}
	fn(&rec)
	rec.UpdatedAt = time.Now().UTC()
	if err := bs.UpsertBuild(ctx, rec); err != nil {
		return BuildRecord{}, fmt.Errorf("persist build %s: %w", in.ID, err)
	}
	fm.publishBuildEvent(rec)
	return rec, nil
}

// publishBuildEvent tells the builder's event stream where the build is. like
// an intervention event it carries ids and states only.
func (fm *FleetManager) publishBuildEvent(rec BuildRecord) {
	if fm.broadcaster == nil || rec.BuilderID == "" {
		return
	}
	fm.mu.RLock()
	v, ok := fm.vms[rec.BuilderID]
	var info VMInfo
	if ok {
		info = v.toInfo()
	}
	fm.mu.RUnlock()
	if !ok {
		return
	}
	fm.broadcaster.publish(EnvironmentEvent{
		ID:        NewEventID(),
		Kind:      "build",
		VMID:      rec.BuilderID,
		State:     info.State,
		URL:       info.URL,
		UpdatedAt: rec.UpdatedAt,
		Build:     &BuildEvent{ID: rec.ID, State: rec.State, Step: rec.Step},
	})
}

// buildSecrets returns in's secrets in plaintext.
func (fm *FleetManager) buildSecrets(in BuildInput) (map[string]string, error) {
	if in.secrets != nil || len(in.Sealed) == 0 {
		return in.secrets, nil
	}
	raw, err := secrets.DecryptBytes(in.Sealed, fm.tokenEncryptionKey)
	if err != nil {
		return nil, errBuildSealed
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errBuildSealed
	}
	return out, nil
}

// buildTaskID is the builder's task id. it is derived from the build id, so a
// retried create step finds the builder an earlier attempt made instead of
// making a second one, and it never collides with the task `fuse up` derives.
func buildTaskID(buildID string) string { return "build-" + buildID }

// planExecs turns the cache lookup's answer into the execs a build runs. hit
// is the deepest step the seed covers, or -1.
//
// cacheable steps are a leading run, so there is one boundary: each step
// before it runs alone and is snapshotted, everything from it on runs as one
// exec, since none of it can be snapshotted. the file block is written on the
// first exec of a cold build only: a seed already contains it. with no
// cacheable step this is one exec of the whole setup phase, file block
// included, which is the build script byte for byte.
func planExecs(keys []string, n, hit int) []BuildExec {
	prefix := min(leadingKeys(keys), n)
	start := hit + 1
	cold := hit < 0
	var out []BuildExec
	for i := start; i < prefix; i++ {
		out = append(out, BuildExec{
			Step:  fmt.Sprintf(buildStepLayerName, i),
			Label: fmt.Sprintf("setup[%d]", i),
			From:  i, To: i + 1,
			Files: cold && i == start,
			Layer: i,
		})
	}
	tail := max(prefix, start)
	if tail < n {
		label := "setup tail"
		if prefix == 0 {
			label = "build phase"
		}
		out = append(out, BuildExec{
			Step: BuildStepTail, Label: label,
			From: tail, To: n,
			Files: cold && tail == start,
			Layer: -1,
		})
	}
	return out
}

// buildArch is the arch a build's layers are filed under when the caller did
// not say: the arch the spec pins, or the one arch every registered host
// shares. a fleet of mixed arches has no safe answer, so "" turns the cache
// off rather than filing a layer under a coin flip.
func (fm *FleetManager) buildArch(spec Spec) string {
	if spec.HostID != "" {
		h, ok := fm.GetHost(spec.HostID)
		if !ok {
			return ""
		}
		return hostLayerArch(h)
	}
	if spec.Arch != "" {
		return spec.Arch
	}
	arch := ""
	for _, h := range fm.ListHosts() {
		a := hostLayerArch(h)
		if arch == "" {
			arch = a
		} else if a != arch {
			return ""
		}
	}
	return arch
}

// hostLayerArch is the arch a layer built on h is filed under: an empty arch
// is amd64, as the snapshot write path assumes.
func hostLayerArch(h Host) string {
	if a := NormalizeArch(h.Capacity.Arch); a != "" {
		return a
	}
	return ArchAMD64
}

// the methods below are the steps of a build, in order. the restate Build
// workflow runs each inside a journaled step and runBuild calls them
// directly, so each must be safe to run again.

// BuildResolveCache finds the deepest setup step whose layer is already
// stored, walking the chain backwards so a warm cache costs one lookup, and
// plans the execs that remain. a lookup that fails builds cold: the cache is
// an optimization and must not cost the build.
func (fm *FleetManager) BuildResolveCache(ctx context.Context, in BuildInput) (BuildPlan, error) {
	n := len(in.Setup.Steps)
	keys := in.Keys
	plan := BuildPlan{Arch: NormalizeArch(in.Arch)}
	if leadingKeys(keys) > 0 && plan.Arch == "" {
		plan.Arch = fm.buildArch(in.Spec)
	}
	if plan.Arch == "" {
		// no arch, no cache: a layer filed under the wrong arch would serve
		// a later build a rootfs it cannot boot.
		keys = nil
	}

	hit := -1
	for i := min(leadingKeys(keys), n) - 1; i >= 0; i-- {
		snap, ok, err := fm.ResolveLayer(ctx, in.TenantID, keys[i], plan.Arch)
		if err != nil {
			fm.logger.WarnContext(ctx, "layer cache lookup failed, building cold", "build", in.ID, "err", err)
			break
		}
		if ok && snap.HostID != "" && snap.Kind != SnapshotKindLive {
			hit = i
			plan.SeedSnapshotID = snap.SnapshotID
			plan.SeedHostID = snap.HostID
			break
		}
	}
	plan.Execs = planExecs(keys, n, hit)

	_, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) {
		rec.Step = BuildStepResolve
		rec.Arch = plan.Arch
		rec.SeedSnapshotID = plan.SeedSnapshotID
		rec.CacheHits = 0
		prefix := leadingKeys(keys)
		for i := range rec.Layers {
			rec.Layers[i].Status = LayerSkip
			rec.Layers[i].Key = ""
			if i < prefix {
				rec.Layers[i].Key = keys[i]
				rec.Layers[i].Status = LayerMiss
			}
			if i <= hit {
				rec.Layers[i].Status = LayerHit
				rec.CacheHits++
			}
		}
	})
	return plan, err
}

// BuildCreateBuilder boots the builder and waits until it runs. its task id
// is the build's, so a builder an earlier attempt made is reused.
func (fm *FleetManager) BuildCreateBuilder(ctx context.Context, in BuildInput, plan BuildPlan) (string, error) {
	taskID := buildTaskID(in.ID)
	if info, ok := fm.GetVMByTask(taskID); ok {
		if info.State != VMStateRunning {
			return "", fmt.Errorf("%w: %s is %s", errBuilderBusy, info.ID, info.State)
		}
		return info.ID, fm.noteBuilder(ctx, in, info.ID)
	}
	if _, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) { rec.Step = BuildStepCreate }); err != nil {
		return "", err
	}
	secretMap, err := fm.buildSecrets(in)
	if err != nil {
		return "", err
	}
	spec := in.Spec
	if plan.SeedSnapshotID != "" {
		// the seed already is the rootfs, and the image it came from is in
		// the layer key, so it replaces the image.
		spec.Image = ""
		spec.SeedSnapshotID = plan.SeedSnapshotID
		spec.PinnedHostID = plan.SeedHostID
	}
	// no startup script: setup runs through exec, for its 600s ceiling and
	// its output.
	info, err := fm.ProvisionAndAssign(ctx, taskID, spec, in.Manifest, secretMap, BootOptions{})
	if err != nil {
		return "", errors.New(secrets.RedactSecretValues(err.Error(), secretMap))
	}
	return info.ID, fm.noteBuilder(ctx, in, info.ID)
}

func (fm *FleetManager) noteBuilder(ctx context.Context, in BuildInput, vmID string) error {
	_, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) { rec.BuilderID = vmID })
	return err
}

// BuildExec runs one setup exec in the builder. a non-zero exit fails the
// build with the redacted tail of its output on the record.
func (fm *FleetManager) BuildExec(ctx context.Context, in BuildInput, vmID string, ex BuildExec) error {
	if _, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) { rec.Step = ex.Step }); err != nil {
		return err
	}
	script := in.Setup.Range(ex.From, ex.To, ex.Files)
	res, err := fm.Exec(ctx, vmID, []string{"sh", "-lc", script}, ExecOptions{Timeout: buildExecTimeout})
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	secretMap, serr := fm.buildSecrets(in)
	if serr != nil {
		secretMap = nil
	}
	output := secrets.RedactSecretValues(string(res.Stdout)+string(res.Stderr), secretMap)
	if len(output) > buildOutputTail {
		output = output[len(output)-buildOutputTail:]
	}
	if _, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) { rec.Output = output }); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s exited %d", errBuildStepFailed, ex.Label, res.ExitCode)
}

// BuildSnapshotLayer snapshots the builder as the layer of setup step i,
// filed under the build's tenant and the step's key.
func (fm *FleetManager) BuildSnapshotLayer(ctx context.Context, in BuildInput, vmID string, i int) (string, error) {
	rec, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{
		Comment:  fmt.Sprintf("fuse build layer %d: %s", i, in.Name),
		Mode:     SnapshotModeBuild,
		LayerKey: in.Keys[i],
		TenantID: in.TenantID,
	})
	if err != nil {
		return "", err
	}
	_, err = fm.updateBuild(ctx, in, func(r *BuildRecord) {
		if i < len(r.Layers) {
			r.Layers[i].SnapshotID = rec.SnapshotID
		}
	})
	return rec.SnapshotID, err
}

// BuildSnapshot takes the build artifact: the builder's disk once setup has
// run.
func (fm *FleetManager) BuildSnapshot(ctx context.Context, in BuildInput, vmID string) (string, error) {
	if _, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) { rec.Step = BuildStepSnapshot }); err != nil {
		return "", err
	}
	rec, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{
		Comment:  "fuse build: " + in.Name,
		Mode:     SnapshotModeBuild,
		Metadata: map[string]string{"name": in.Name},
	})
	if err != nil {
		return "", err
	}
	_, err = fm.updateBuild(ctx, in, func(r *BuildRecord) { r.SnapshotID = rec.SnapshotID })
	return rec.SnapshotID, err
}

// BuildDestroyBuilder destroys the builder, unless the build keeps it. it runs
// on success and as the compensation of a failure, so it finds the builder by
// its task id: a create that failed part way may have left one. outcome is
// how the build ended (BuildSucceeded or BuildFailed): the builder's stream
// closes with the builder, so its last build event carries the outcome before
// the destroy rather than after it.
func (fm *FleetManager) BuildDestroyBuilder(ctx context.Context, in BuildInput, outcome string) error {
	if in.Keep {
		return nil
	}
	info, ok := fm.GetVMByTask(buildTaskID(in.ID))
	if !ok {
		return nil
	}
	rec, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) { rec.Step = BuildStepDestroy })
	if err != nil {
		return err
	}
	rec.State = outcome
	fm.publishBuildEvent(rec)
	if err := fm.DestroyVM(ctx, info.ID); err != nil && !errors.Is(err, ErrVMNotFound) {
		return err
	}
	return nil
}

// BuildFinish records a build that produced artifact snapshotID. the id comes
// from the snapshot step's result, not the record, so a record that had to
// start again still names the artifact.
func (fm *FleetManager) BuildFinish(ctx context.Context, in BuildInput, snapshotID string) error {
	_, err := fm.updateBuild(ctx, in, func(rec *BuildRecord) {
		now := time.Now().UTC()
		rec.SnapshotID = snapshotID
		rec.State = BuildSucceeded
		rec.Step = BuildStepDone
		rec.FinishedAt = &now
	})
	if err == nil {
		fm.appendEvent(ctx, "build", in.ID, "build.succeeded", nil)
	}
	return err
}

// BuildFailed records a build that failed with cause. layers it already
// snapshotted stay in the store, so a retry after a fixed setup line resumes
// from the last good one.
func (fm *FleetManager) BuildFailed(ctx context.Context, in BuildInput, cause string) error {
	secretMap, err := fm.buildSecrets(in)
	if err != nil {
		secretMap = nil
	}
	cause = secrets.RedactSecretValues(cause, secretMap)
	_, err = fm.updateBuild(ctx, in, func(rec *BuildRecord) {
		now := time.Now().UTC()
		rec.State = BuildFailed
		rec.Error = cause
		rec.FinishedAt = &now
	})
	if err == nil {
		fm.appendEvent(ctx, "build", in.ID, "build.failed", map[string]any{"error": cause})
	}
	return err
}
