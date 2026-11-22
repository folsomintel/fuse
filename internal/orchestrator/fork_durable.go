package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DestroyReasonForkRollback is the reason on the destroy a failed durable
// fork sends for its new vm.
const DestroyReasonForkRollback = "fork_rollback"

// CleanupOfCreatedVM reports whether a destroy with reason is one a create
// or fork sends for a vm it created and then gave up on. such a destroy names
// the vm's host, and an empty one means the default provider of a fleet with
// no registered hosts.
func CleanupOfCreatedVM(reason string) bool {
	return reason == DestroyReasonCreateCleanup || reason == DestroyReasonForkRollback
}

// forkTaskPrefix starts every fork's task id, and so its new vm's id.
const forkTaskPrefix = "fork-"

// ForkRefusals are the sentinel errors a fork refuses with besides
// SnapshotFleetErrors. the durable handler carries each one across restate as
// its own error code, so the api answers it with that sentinel's status. a
// code is its index, so new sentinels go at the end.
var ForkRefusals = []error{
	ErrSnapshotNotSeedable,
	// placement found no host with room (ErrSeedUnplaceable wraps it).
	ErrNoCapacity,
}

// ForkRequest is the input of the durable Environment/{vm_id}.Fork handler,
// keyed by the fork's new vm id.
type ForkRequest struct {
	SourceVMID      string `json:"source_vm_id"`
	TaskID          string `json:"task_id"`
	Comment         string `json:"comment,omitempty"`
	ReuseSnapshotID string `json:"reuse_snapshot_id,omitempty"`
	// Live resumes the fork from a live seed's memory image: the seed the
	// handler takes is live, and a reused one must be.
	Live bool `json:"live,omitempty"`
}

// ForkPlan is what ForkBegin settled. every later step reads it from the
// journal, so a step after a restart works on the same fork.
type ForkPlan struct {
	SourceVMID   string `json:"source_vm_id"`
	SourceHostID string `json:"source_host_id,omitempty"`
	NewVMID      string `json:"new_vm_id"`
	TaskID       string `json:"task_id"`
	Comment      string `json:"comment,omitempty"`
	Live         bool   `json:"live,omitempty"`
	// Seed is the reused snapshot the fork boots from. empty when the fork
	// snapshots the source itself.
	Seed      SnapshotRecord `json:"seed"`
	CreatedAt time.Time      `json:"created_at"`
}

// DurableForker runs a fork as the restate Environment/{vm_id}.Fork handler,
// keyed by the new vm's id, and waits for it. the fork keeps running if ctx
// ends first. *durable.Client satisfies it.
type DurableForker interface {
	Fork(ctx context.Context, vmID string, req ForkRequest) error
}

// SetDurableForker routes forks through restate. set it before Start.
func (fm *FleetManager) SetDurableForker(d DurableForker) {
	fm.mu.Lock()
	fm.durableFork = d
	fm.mu.Unlock()
}

// durableForker is the forker forks go through.
func (fm *FleetManager) durableForker() DurableForker {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableFork
}

// isForkPlaceholder reports whether a recovered provisioning vm is a fork
// restate is still running. the fork either finishes it or destroys it, so
// recovery leaves it provisioning.
func (fm *FleetManager) isForkPlaceholder(v *vm) bool {
	return strings.HasPrefix(v.taskID, forkTaskPrefix)
}

// MaxForkCount bounds the children one fork request makes.
const MaxForkCount = 1000

// forkFanout is how many of a fork's children run at once.
const forkFanout = 8

// forkChildren is ForkEnvironment on restate: one durable fork per child,
// each keyed by its own new vm id. a single child takes its seed inside its
// handler, as a fork always has. several share one seed, taken here once
// (or reused) and handed to every child as its reuse_snapshot_id, so a
// child's rollback never deletes it. a failed child does not undo its
// siblings: the ids returned are the children that forked, and the error
// names the first failure. the seed this took is deleted only when every
// child failed for good.
func (fm *FleetManager) forkChildren(ctx context.Context, d DurableForker, srcVMID string, opts ForkOptions) ([]string, error) {
	count := max(opts.Count, 1)
	if count > MaxForkCount {
		return nil, fmt.Errorf("fork count %d is over the limit of %d", count, MaxForkCount)
	}
	fm.mu.RLock()
	_, ok := fm.vms[srcVMID]
	fm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, srcVMID)
	}
	key := IdempotencyKey(ctx)
	req := ForkRequest{Comment: opts.Comment, ReuseSnapshotID: opts.ReuseSnapshotID, Live: opts.Live}
	if count == 1 {
		id, err := fm.forkChild(ctx, d, srcVMID, key, 0, req)
		if err != nil {
			return nil, err
		}
		return []string{id}, nil
	}

	// a retry of a fan-out whose children already exist replays them, so it
	// must not be refused for capacity those very children now hold
	replay := false
	if key != "" {
		fm.mu.RLock()
		for i := range count {
			if _, ok := fm.vms[fm.prefix+forkTaskID(srcVMID, key, i)]; ok {
				replay = true
				break
			}
		}
		fm.mu.RUnlock()
	}
	seed, taken, err := fm.forkSeed(ctx, srcVMID, opts, replay)
	if err != nil {
		return nil, err
	}
	req = ForkRequest{ReuseSnapshotID: seed.SnapshotID, Live: opts.Live}
	ids := make([]string, count)
	errs := make([]error, count)
	sem := make(chan struct{}, forkFanout)
	var wg sync.WaitGroup
	for i := range count {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ids[i], errs[i] = fm.forkChild(ctx, d, srcVMID, key, i, req)
		}()
	}
	wg.Wait()

	var (
		forked  []string
		first   error
		firstAt int
		settled = true
	)
	for i, err := range errs {
		if err == nil {
			forked = append(forked, ids[i])
			continue
		}
		if first == nil {
			first, firstAt = err, i
		}
		// a child restate has not settled may still be running on the seed.
		settled = settled && IsSettled(err)
	}
	if first == nil {
		return forked, nil
	}
	if len(forked) == 0 && taken && settled {
		fm.deleteCheckpoint(ctx, seed)
	}
	// with survivors, or with a child restate has not settled (it may still
	// fork), the failure is not reported settled, even when restate recorded
	// it: a settled 5xx moves the caller's retry to a new restate key, and so
	// to a new set of children, stranding the ones that forked. kept on the
	// same key, a retry replays every child's first answer instead.
	var s *SettledError
	if (len(forked) > 0 || !settled) && errors.As(first, &s) {
		first = s.Err
	}
	return forked, fmt.Errorf("fork %s: %d of %d children failed (forked: [%s]); child %d: %w",
		srcVMID, count-len(forked), count, strings.Join(forked, ", "), firstAt, first)
}

// forkSeed is the one seed a multi-child fork shares: the reused snapshot, or
// a snapshot of the source taken here (taken). it runs the fork's checks
// first, so a fork every child would refuse does not pause the source. the
// snapshot goes through the source's Environment.Snapshot under the
// request's idempotency key, so a retry gets the same seed back instead of
// taking another. a replay skips the placement check: its children are
// already placed.
func (fm *FleetManager) forkSeed(ctx context.Context, srcVMID string, opts ForkOptions, replay bool) (seed SnapshotRecord, taken bool, err error) {
	src, existing, err := fm.checkFork(ctx, srcVMID, opts.ReuseSnapshotID, opts.Live)
	if err != nil {
		return SnapshotRecord{}, false, err
	}
	if !replay {
		if err := fm.forkFitsSomewhere(src, opts.Live); err != nil {
			return SnapshotRecord{}, false, err
		}
	}
	if opts.ReuseSnapshotID != "" {
		return existing, false, nil
	}
	wctx, cancel := durableWait(ctx)
	defer cancel()
	seed, err = fm.durableSnapshotsClient().Snapshot(wctx, srcVMID, SnapshotOptions{Comment: opts.Comment, Live: opts.Live})
	if err != nil {
		return SnapshotRecord{}, false, err
	}
	if err := forkSeedKind(seed, opts.Live); err != nil {
		fm.deleteCheckpoint(ctx, seed)
		return SnapshotRecord{}, false, err
	}
	return seed, true, nil
}

// forkSeedKind refuses a live fork's seed that came back a disk snapshot: a
// host agent too old to take live snapshots answers a live request with a
// disk one, and nothing could resume from it.
func forkSeedKind(seed SnapshotRecord, live bool) error {
	if live && seed.Kind != SnapshotKindLive {
		return fmt.Errorf("%w: seed snapshot %s came back a %s snapshot, with no memory image for a live fork to resume; the source's host agent cannot take live snapshots",
			ErrSnapshotNotSeedable, seed.SnapshotID, seed.Kind)
	}
	return nil
}

// forkChild runs child i of a fork through d and waits for it.
func (fm *FleetManager) forkChild(ctx context.Context, d DurableForker, srcVMID, key string, i int, req ForkRequest) (string, error) {
	taskID := forkTaskID(srcVMID, key, i)
	newVMID := fm.prefix + taskID
	req.SourceVMID, req.TaskID = srcVMID, taskID
	ctx, cancel := durableWait(ctx)
	defer cancel()
	if err := d.Fork(ctx, newVMID, req); err != nil {
		return "", err
	}
	return newVMID, nil
}

// forkTaskID names child i of a fork. a retry carrying an idempotency key
// derives the same names, so restate attaches each child to the invocation
// the first request started instead of forking again. child 0 keeps the name
// a single fork always had.
func forkTaskID(srcVMID, idempotencyKey string, i int) string {
	if idempotencyKey == "" {
		return forkTaskPrefix + NewEventID()
	}
	in := srcVMID + "\x00" + idempotencyKey
	if i > 0 {
		in += "\x00" + strconv.Itoa(i)
	}
	sum := sha256.Sum256([]byte(in))
	return forkTaskPrefix + hex.EncodeToString(sum[:16])
}

// forkTarget is the fork's new vm, which the placeholder step tracked.
func (fm *FleetManager) forkTarget(plan ForkPlan) (*vm, error) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[plan.NewVMID]
	if !ok {
		return nil, fmt.Errorf("%w: fork %s is no longer tracked", ErrVMNotFound, plan.NewVMID)
	}
	return v, nil
}

// forkTargetEnv is the new vm's environment handle. a step after a restart
// finds none in memory and looks the vm up on its host.
func (fm *FleetManager) forkTargetEnv(ctx context.Context, plan ForkPlan) (*vm, Environment, error) {
	v, err := fm.forkTarget(plan)
	if err != nil {
		return nil, nil, err
	}
	fm.mu.RLock()
	env, hostID := v.env, v.hostID
	fm.mu.RUnlock()
	if env != nil {
		return v, env, nil
	}
	env, err = fm.migrateProvider(hostID).Get(ctx, plan.NewVMID)
	if err != nil {
		return nil, nil, fmt.Errorf("look up forked vm %s on host %s: %w", plan.NewVMID, hostID, err)
	}
	fm.mu.Lock()
	v.env = env
	fm.mu.Unlock()
	return v, env, nil
}

// the methods below are the steps of a durable fork, in order, and then its
// compensation. each runs inside a journaled restate step and must be safe to
// run again: a crash after a step's effect and before its journal entry
// replays it. the seed snapshot is not among them: the handler takes it
// through the source's own Environment.Snapshot, so it is serialized with the
// source's other ops.

// ForkBegin checks that the source can be forked as asked. it writes
// nothing, so what it refuses a retry refuses too.
func (fm *FleetManager) ForkBegin(ctx context.Context, vmID string, req ForkRequest) (ForkPlan, error) {
	src, seed, err := fm.checkFork(ctx, req.SourceVMID, req.ReuseSnapshotID, req.Live)
	if err != nil {
		return ForkPlan{}, err
	}
	// a fork no host has room for is refused here, before the seed snapshot
	// pauses the source, so a retried burst does not snapshot it each time.
	// place still runs the full check (it also knows whether the seed moves).
	if err := fm.forkFitsSomewhere(src, req.Live); err != nil {
		return ForkPlan{}, err
	}
	return ForkPlan{
		SourceVMID:   req.SourceVMID,
		SourceHostID: src.hostID,
		NewVMID:      vmID,
		TaskID:       req.TaskID,
		Comment:      req.Comment,
		Live:         req.Live,
		Seed:         seed,
		CreatedAt:    time.Now(),
	}, nil
}

// ForkPlaceholder tracks and persists the fork's new vm, provisioning,
// before anything exists for it on a host: reconcile destroys a host vm the
// fleet does not track, and a restart has to find the fork to finish or roll
// it back. a retry finds the placeholder already there.
func (fm *FleetManager) ForkPlaceholder(ctx context.Context, plan ForkPlan) error {
	fm.mu.Lock()
	if _, ok := fm.vms[plan.NewVMID]; !ok {
		src, ok := fm.vms[plan.SourceVMID]
		if !ok {
			fm.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrVMNotFound, plan.SourceVMID)
		}
		spec := src.spec
		spec.Name = plan.NewVMID
		// a fork inherits the source's shape, never its seed or placement:
		// see forkEnvironment.
		spec.SeedSnapshotID = ""
		spec.PinnedHostID = ""
		spec.HostID = ""
		spec.Labels = nil
		spec.Ancestors = append(slices.Clone(src.spec.Ancestors), plan.SourceVMID)
		fm.vms[plan.NewVMID] = &vm{
			id:                plan.NewVMID,
			state:             VMStateProvisioning,
			taskID:            plan.TaskID,
			spec:              spec,
			createdAt:         plan.CreatedAt,
			updatedAt:         plan.CreatedAt,
			lastActivityAt:    plan.CreatedAt,
			hitlKeys:          src.hitlKeys,
			hitlKeysEncrypted: src.hitlKeysEncrypted,
		}
	}
	fm.mu.Unlock()
	if err := fm.persistVMByID(ctx, plan.NewVMID); err != nil {
		return fmt.Errorf("persist fork %s placeholder: %w", plan.NewVMID, err)
	}
	return nil
}

// ForkPlace picks the fork's host (see placeFork), charges it there and
// persists the placement, so a restart re-derives the charge. a fork already
// placed keeps its host. a fork that fits nowhere fails with ErrNoCapacity.
func (fm *FleetManager) ForkPlace(ctx context.Context, plan ForkPlan, seed SnapshotRecord) (string, error) {
	v, err := fm.forkTarget(plan)
	if err != nil {
		return "", err
	}
	fm.mu.RLock()
	hostID, spec := v.hostID, v.spec
	fm.mu.RUnlock()
	if hostID != "" {
		return hostID, nil
	}
	if err := forkSeedKind(seed, plan.Live); err != nil {
		return "", err
	}
	srcProvider, err := fm.providerForVM(plan.SourceHostID)
	if err != nil {
		return "", err
	}
	hostID, _, err = fm.placeFork(ctx, spec, plan.SourceHostID, srcProvider, seed, plan.Live)
	if err != nil {
		return "", err
	}
	if hostID == "" {
		// single-provider mode: there is no host to charge.
		return "", nil
	}
	// placeFork judged a copy of the host; re-check the live one in the
	// section that charges it, so a concurrent fork or create that took the
	// last slot meanwhile turns this one into a retry-later refusal.
	fm.mu.Lock()
	h, ok := fm.hosts[hostID]
	if !ok {
		fm.mu.Unlock()
		return "", fmt.Errorf("%w: host %s was removed while the fork was placed", ErrNoCapacity, hostID)
	}
	if why := hostRejection(h, spec); why != "" {
		fm.mu.Unlock()
		return "", fmt.Errorf("%w: host %s %s", ErrNoCapacity, hostID, why)
	}
	v.hostID = hostID
	fm.allocateOnHost(hostID, v)
	fm.mu.Unlock()
	if err := fm.persistVMByID(ctx, plan.NewVMID); err != nil {
		fm.mu.Lock()
		fm.deallocateOnHost(hostID, v.spec)
		v.hostID = ""
		fm.mu.Unlock()
		return "", fmt.Errorf("persist fork %s placement: %w", plan.NewVMID, err)
	}
	return hostID, nil
}

// ForkArtifact copies the seed to hostID, a host other than the source's, and
// returns the id of the copy there. a copy that already landed is found
// again.
func (fm *FleetManager) ForkArtifact(ctx context.Context, plan ForkPlan, seed SnapshotRecord, hostID string) (string, error) {
	if _, err := fm.forkTarget(plan); err != nil {
		return "", err
	}
	return fm.ensureArtifactOnHost(ctx, seed, hostID, false)
}

// ForkCreate creates the fork on its host: from the seed checkpoint on the
// source's host, or from localID, the seed's copy, anywhere else. a live fork
// is always the latter, resumed from the copy's memory image. the create is
// keyed on the step, so a retry gets the agent's first answer back.
func (fm *FleetManager) ForkCreate(ctx context.Context, plan ForkPlan, seed SnapshotRecord, hostID, localID string) error {
	v, err := fm.forkTarget(plan)
	if err != nil {
		return err
	}
	if liveForkOffSource(plan.Live, hostID, plan.SourceHostID) {
		// placeFork never picks it; a resume here would rmtree the source's
		// vm dir (see liveForkOffSource).
		return errLiveForkNoHost(plan.SourceHostID, "the fork was placed there")
	}
	var env Environment
	if hostID == plan.SourceHostID {
		forkable, ok := fm.migrateProvider(plan.SourceHostID).(SnapshotForkable)
		if !ok {
			return fmt.Errorf("provider does not support fork for vm %s", plan.SourceVMID)
		}
		fm.mu.RLock()
		spec := v.spec
		fm.mu.RUnlock()
		env, err = forkable.CreateFromCheckpoint(ctx, spec, plan.SourceVMID, seed.SnapshotID)
	} else {
		// only these two fields, never the whole spec: placement may have
		// bound resources onto v.spec.
		fm.mu.Lock()
		v.spec.SeedSnapshotID = localID
		v.spec.PinnedHostID = hostID
		v.spec.ResumeSeed = plan.Live
		spec := v.spec
		fm.mu.Unlock()
		fm.touchArtifact(localID, seed.SnapshotID)
		env, err = fm.migrateProvider(hostID).Create(ctx, spec)
	}
	if err != nil {
		// the host agent's 409 is a resume it refuses (another cpu or
		// firecracker build, see resume_plan).
		var httpErr *HTTPStatusError
		if plan.Live && errors.As(err, &httpErr) && httpErr.Code == http.StatusConflict {
			return fmt.Errorf("%w: host %s cannot resume a live fork of vm %s: %w", ErrSnapshotNotSeedable, hostID, plan.SourceVMID, err)
		}
		return fmt.Errorf("fork vm %s from snapshot %s: %w", plan.SourceVMID, seed.SnapshotID, err)
	}
	fm.mu.Lock()
	v.env = env
	fm.mu.Unlock()
	return nil
}

// ForkIngress publishes the fork under ports of its own, returned sealed. a
// retry publishes the same owner again.
func (fm *FleetManager) ForkIngress(ctx context.Context, plan ForkPlan) ([]byte, error) {
	v, err := fm.forkTarget(plan)
	if err != nil {
		return nil, err
	}
	fm.mu.RLock()
	hostID, spec := v.hostID, v.spec
	fm.mu.RUnlock()
	if !fm.ingressSupported(hostID, spec) {
		return nil, nil
	}
	p, err := fm.publishIngress(ctx, plan.NewVMID, nil, "")
	if err != nil {
		return nil, err
	}
	return fm.sealIngress(p)
}

// ForkCredentials makes the fork credentials of its own, returned sealed: it
// booted a copy of the source's disk, which holds the source's. made once, in
// a step of their own, so a retried upload sends the same files.
func (fm *FleetManager) ForkCredentials(_ context.Context, plan ForkPlan) (RotatedCredentials, error) {
	return fm.cloneCredentials(plan.NewVMID, "forked")
}

// ForkUpload writes the fork's tunnel file and credential files into its
// guest.
func (fm *FleetManager) ForkUpload(ctx context.Context, plan ForkPlan, started MigrateStarted) error {
	_, env, err := fm.forkTargetEnv(ctx, plan)
	if err != nil {
		return err
	}
	return fm.uploadCloneFiles(ctx, env, plan.NewVMID, "forked", started)
}

// ForkStartAgent restarts the fork's guest agent with its own credentials:
// until it does, fused keeps serving the source's token.
func (fm *FleetManager) ForkStartAgent(ctx context.Context, plan ForkPlan, started MigrateStarted) error {
	_, env, err := fm.forkTargetEnv(ctx, plan)
	if err != nil {
		return err
	}
	return fm.restartCloneAgent(ctx, env, plan.NewVMID, "forked", started)
}

// ForkRunning records the fork running. from here nothing undoes it.
func (fm *FleetManager) ForkRunning(ctx context.Context, plan ForkPlan, started MigrateStarted) error {
	v, env, err := fm.forkTargetEnv(ctx, plan)
	if err != nil {
		return err
	}
	fm.mu.RLock()
	done := v.state == VMStateRunning
	fm.mu.RUnlock()
	if done {
		return nil
	}
	if _, err := fm.migrateToken(env, started); err != nil {
		return err
	}
	ip, err := fm.openIngress(started.Ingress)
	if err != nil {
		return err
	}
	fm.mu.Lock()
	v.state = VMStateRunning
	v.env = env
	v.url = env.URL()
	v.authTokenEncrypted = started.Credentials.TokenEncrypted
	v.drainCommand = DefaultFusedDrainCommand
	v.updatedAt = time.Now()
	if ip != nil {
		v.applyIngress(ip.grant)
	}
	fm.mu.Unlock()
	if err := fm.persistVMByID(ctx, plan.NewVMID); err != nil {
		// a retry still sees a vm that is not running yet.
		fm.mu.Lock()
		v.state = VMStateProvisioning
		fm.mu.Unlock()
		return fmt.Errorf("persist forked vm %s running state: %w", plan.NewVMID, err)
	}
	fm.publishStateChange(plan.NewVMID, "")
	return nil
}

// ForkTask records the fork's task running. a failure is logged: the vm
// itself is recorded already.
func (fm *FleetManager) ForkTask(ctx context.Context, plan ForkPlan) error {
	if fm.store == nil {
		return nil
	}
	if err := fm.store.UpsertTask(ctx, TaskRecord{
		TaskID:     plan.TaskID,
		VMID:       plan.NewVMID,
		RunStatus:  TaskRunRunning,
		AssignedAt: plan.CreatedAt,
		UpdatedAt:  plan.CreatedAt,
	}); err != nil {
		fm.logger.WarnContext(ctx, "persist forked task running state failed", "vm", plan.NewVMID, "task", plan.TaskID, "err", err)
	}
	return nil
}

// ForkLineage records the fork's descent from its seed and the vm.forked
// event. the record's id comes from the fork's, so a retry writes the same
// row. a failed record is logged.
func (fm *FleetManager) ForkLineage(ctx context.Context, plan ForkPlan, seed SnapshotRecord) error {
	fm.mu.RLock()
	hostID := ""
	if v, ok := fm.vms[plan.NewVMID]; ok {
		hostID = v.hostID
	}
	fm.mu.RUnlock()
	lineageMeta, _ := marshalSnapshotMetadata("", nil)
	lineage := SnapshotRecord{
		SnapshotID:       "fork-seed-" + strings.TrimPrefix(plan.TaskID, forkTaskPrefix),
		VMID:             plan.NewVMID,
		TaskID:           plan.TaskID,
		HostID:           hostID,
		TenantID:         snapshotTenantID(plan.TaskID, plan.NewVMID),
		ParentSnapshotID: seed.SnapshotID,
		Mode:             SnapshotModeAuto,
		State:            SnapshotStateReady,
		Metadata:         lineageMeta,
		CreatedAt:        plan.CreatedAt,
		UpdatedAt:        plan.CreatedAt,
	}
	if err := fm.upsertSnapshotRecord(ctx, lineage); err != nil {
		fm.logger.WarnContext(ctx, "persist fork lineage record failed", "vm", plan.NewVMID, "parent", seed.SnapshotID, "err", err)
	}
	fm.appendEvent(ctx, "vm", plan.NewVMID, "vm.forked", map[string]any{
		"source_vm_id":     plan.SourceVMID,
		"seed_snapshot_id": seed.SnapshotID,
	})
	return nil
}

// the compensation of a failed fork, in order. the handler then sends the new
// vm's destroy through Environment/{vm_id}.Destroy, which restate runs once
// the fork has ended.

// ForkUnpublishIngress removes the routes the fork published.
func (fm *FleetManager) ForkUnpublishIngress(ctx context.Context, plan ForkPlan) error {
	fm.unpublishIngress(ctx, plan.NewVMID)
	return nil
}

// ForkAbandon forgets the fork: its row, its placeholder and the capacity it
// was charged. a vm that is no longer the fork's provisioning placeholder is
// left alone.
func (fm *FleetManager) ForkAbandon(ctx context.Context, plan ForkPlan, cause string) error {
	fm.mu.RLock()
	v, ok := fm.vms[plan.NewVMID]
	ours := ok && v.state == VMStateProvisioning && v.taskID == plan.TaskID
	fm.mu.RUnlock()
	if ok && !ours {
		return nil
	}
	if fm.store != nil {
		if err := fm.store.DeleteVM(ctx, plan.NewVMID); err != nil {
			return fmt.Errorf("delete fork %s row: %w", plan.NewVMID, err)
		}
	}
	if ours {
		fm.mu.Lock()
		if v.hostID != "" {
			fm.deallocateOnHost(v.hostID, v.spec)
		}
		delete(fm.vms, plan.NewVMID)
		fm.mu.Unlock()
	}
	fm.logger.ErrorContext(ctx, "fork failed", "vm", plan.NewVMID, "source", plan.SourceVMID, "err", cause)
	return nil
}

// ForkDeleteSeed deletes the seed snapshot a failed fork took. a failure is
// logged and left to retention.
func (fm *FleetManager) ForkDeleteSeed(ctx context.Context, seed SnapshotRecord) error {
	fm.deleteCheckpoint(ctx, seed)
	return nil
}
