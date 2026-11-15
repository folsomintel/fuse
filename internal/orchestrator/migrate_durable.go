package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// DestroyReasonMigrated is the reason on the destroy of a migrated vm's
// source, DestroyReasonMigrateRollback on that of a target a failed migrate
// leaves behind.
const (
	DestroyReasonMigrated        = "migrated"
	DestroyReasonMigrateRollback = "migrate_rollback"
)

// MigrateRefusals are the sentinel errors a migrate fails with, besides
// SnapshotFleetErrors, that the api answers with a status of their own. the
// durable handler carries each one across restate as its own error code.
var MigrateRefusals = []error{
	ErrLiveMigrateRefused,
	ErrNoCapacity,
	ErrNoHosts,
	ErrHostPinUnschedulable,
	ErrNoLabelMatch,
}

// MigrateRequest is the input of the durable Environment/{vm_id}.Migrate
// handler.
type MigrateRequest struct {
	TargetHostID string `json:"target_host_id,omitempty"`
	Live         bool   `json:"live,omitempty"`
	Lazy         bool   `json:"lazy,omitempty"`
}

// MigratePlan is what MigrateBegin settled. every later step reads it from
// the journal, so it carries ids, never handles.
type MigratePlan struct {
	SourceVMID   string `json:"source_vm_id"`
	SourceHostID string `json:"source_host_id,omitempty"`
	TargetHostID string `json:"target_host_id,omitempty"`
	// MigrationID names the migrate. it is also the new vm's task id, and
	// the key of the Migrate object that watches a lazy migrate's memory.
	MigrationID string `json:"migration_id"`
	NewVMID     string `json:"new_vm_id"`
	Live        bool   `json:"live,omitempty"`
	Lazy        bool   `json:"lazy,omitempty"`
	// Delta is a live, non-lazy migrate from a source that can take diff
	// snapshots: the bulk is copied with the guest running, then only the
	// diff with it paused.
	Delta     bool      `json:"delta,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SameHost reports whether the migrate stays on the source's host, which only
// a vm with no host (single-provider mode) does.
func (p MigratePlan) SameHost() bool {
	return p.TargetHostID == p.SourceHostID
}

// MigrateStarted is what the ingress and credential steps leave for the
// steps after them. it is journaled, so it carries ciphertext only.
type MigrateStarted struct {
	// Ingress is the target's ingress publish, sealed under the token
	// encryption key. nil when the target is not published.
	Ingress []byte `json:"ingress,omitempty"`
	// Credentials are the target's own, as MigrateCredentials sealed them.
	// zero with no encryption key, where the guest runs without credentials.
	Credentials RotatedCredentials `json:"credentials"`
}

// MemoryWatch is the input of Migrate/{migration_id}.WatchMemory: the lazily
// resumed vm and the seed it pages its memory from.
type MemoryWatch struct {
	VMID   string `json:"vm_id"`
	SeedID string `json:"seed_id"`
}

// MemoryPoll is one look at a lazily resumed vm's memory transfer.
type MemoryPoll struct {
	Status MemoryStatus `json:"status"`
	// Gone is a vm that is destroyed or cannot be watched: nothing depends on
	// its seed any more.
	Gone bool `json:"gone,omitempty"`
}

// Settled reports whether the transfer is over, one way or another.
func (p MemoryPoll) Settled() bool {
	return p.Gone || p.Status.Done || p.Status.Error != ""
}

// LazyMemoryHolder is the ArtifactPin holder that keeps a lazy migrate's seed
// while vmID still pages from it.
func LazyMemoryHolder(vmID string) string {
	return "lazy:" + vmID
}

// MigrateMetrics is an optional extension of ReconcileMetrics, timing how long
// a delta migrate kept its source paused. outcome is "cutover" when the
// target took over and "rolled_back" when the source was resumed.
type MigrateMetrics interface {
	MigrateFrozen(outcome string, d time.Duration)
}

// DurableMigrator runs a migrate as the restate Environment/{vm_id}.Migrate
// handler and waits for it, returning the new vm's id. *durable.Client
// satisfies it.
type DurableMigrator interface {
	Migrate(ctx context.Context, vmID string, req MigrateRequest) (string, error)
}

// SetDurableMigrator routes migrates through restate. nil (mode off, the
// default) keeps them in process. set it before Start.
func (fm *FleetManager) SetDurableMigrator(d DurableMigrator) {
	fm.mu.Lock()
	fm.durableMigrate = d
	fm.mu.Unlock()
}

func (fm *FleetManager) durableMigrator() DurableMigrator {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableMigrate
}

// durableMigrateVM is migrateVM in mode self or cloud. a caller that gives up
// does not stop the migrate.
func (fm *FleetManager) durableMigrateVM(ctx context.Context, d DurableMigrator, vmID string, opts MigrateOptions) (string, error) {
	fm.mu.Lock()
	if _, ok := fm.vms[vmID]; !ok {
		fm.mu.Unlock()
		return "", fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if fm.migrating == nil {
		fm.migrating = make(map[string]int)
	}
	fm.migrating[vmID]++
	fm.mu.Unlock()
	defer func() {
		fm.mu.Lock()
		if fm.migrating[vmID]--; fm.migrating[vmID] <= 0 {
			delete(fm.migrating, vmID)
		}
		fm.mu.Unlock()
	}()
	ctx, cancel := durableWait(ctx)
	defer cancel()
	return d.Migrate(ctx, vmID, MigrateRequest(opts))
}

// isMigratePlaceholder reports whether a recovered provisioning vm is the
// target of a migrate restate is still running. the migrate either finishes
// it or destroys it, so recovery leaves it provisioning.
func (fm *FleetManager) isMigratePlaceholder(v *vm) bool {
	return fm.durableMigrator() != nil && strings.HasPrefix(v.taskID, migrateTaskPrefix)
}

// migrateTaskPrefix starts every migrate's task id, and so its new vm's id.
const migrateTaskPrefix = "migrate-"

// migrateProvider is the provider of a vm on hostID, the fleet's own when the
// host has none.
func (fm *FleetManager) migrateProvider(hostID string) Provider {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	if hostID != "" {
		if p, ok := fm.providerForHost(hostID); ok {
			return p
		}
	}
	return fm.provider
}

// the methods below are the steps of a migrate. the durable handler runs each
// inside a journaled restate step, and must be safe to run again; the
// in-process path calls them in turn.

// MigrateBegin checks that vmID can be migrated as asked, picks the target
// and names the new vm. it reads the fleet only, so what it refuses a retry
// refuses too.
func (fm *FleetManager) MigrateBegin(ctx context.Context, vmID string, req MigrateRequest) (MigratePlan, error) {
	targetHostID := req.TargetHostID
	fm.mu.RLock()
	src, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return MigratePlan{}, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	state := src.state
	srcSpec := src.spec
	srcHostID := src.hostID
	srcEnv := src.env

	srcBackend := HostBackend("")
	if h, ok := fm.hosts[srcHostID]; ok {
		srcBackend = h.Backend
	}
	if targetHostID != "" {
		targetBackend := HostBackend("")
		if h, ok := fm.hosts[targetHostID]; ok {
			targetBackend = h.Backend
		}
		if targetBackend == "" {
			fm.mu.RUnlock()
			return MigratePlan{}, fmt.Errorf("%w: target host %s is not registered", ErrArtifactImmovable, targetHostID)
		}
		if srcBackend != "" && targetBackend != srcBackend {
			fm.mu.RUnlock()
			return MigratePlan{}, fmt.Errorf("source host %s runs %s backend, target host %s runs %s backend: cross-backend migration is not supported", srcHostID, srcBackend, targetHostID, targetBackend)
		}
	}
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return MigratePlan{}, fmt.Errorf("vm %s in state %s: migrate requires running", vmID, state)
	}

	if srcSpec.GPUs > 0 {
		return MigratePlan{}, fmt.Errorf("%w: vm %s has a gpu passthrough device: migrate is not supported for gpu environments", ErrGPUUnsupported, vmID)
	}

	if _, ok := fm.migrateProvider(srcHostID).(SnapshotForkable); !ok {
		return MigratePlan{}, fmt.Errorf("provider does not support migrate for vm %s", vmID)
	}

	// an empty target means the orchestrator picks one. a vm with no host is
	// on the single-provider path, where there is nowhere else to go.
	if targetHostID == "" && srcHostID != "" {
		picked, err := fm.pickMigrateTarget(vmID, srcHostID, srcBackend, srcSpec)
		if err != nil {
			return MigratePlan{}, err
		}
		targetHostID = picked
	}

	if req.Live && (targetHostID == "" || targetHostID == srcHostID) {
		return MigratePlan{}, fmt.Errorf("%w: it needs a target host other than the source's (%s)", ErrLiveMigrateRefused, srcHostID)
	}
	if req.Lazy && !req.Live {
		return MigratePlan{}, fmt.Errorf("%w: lazy needs live", ErrLiveMigrateRefused)
	}
	if req.Lazy && !srcSpec.HugePages {
		return MigratePlan{}, fmt.Errorf("%w: vm %s was not booted with huge pages, which a lazy migrate needs", ErrLiveMigrateRefused, vmID)
	}

	// a live, non-lazy migrate from a source that can take diff snapshots
	// copies the bulk while the guest keeps running, then pauses it only for
	// the pages and blocks that changed since, so nothing the guest writes
	// during the copy is lost and the pause is as long as the diff, not the
	// whole image.
	_, srcDelta := srcEnv.(DeltaSnapshotCapable)
	migrationID := migrateTaskPrefix + NewEventID()
	return MigratePlan{
		SourceVMID:   vmID,
		SourceHostID: srcHostID,
		TargetHostID: targetHostID,
		MigrationID:  migrationID,
		NewVMID:      fm.prefix + migrationID,
		Live:         req.Live,
		Lazy:         req.Lazy,
		Delta:        req.Live && !req.Lazy && srcDelta && fm.artifactMover != nil,
		CreatedAt:    time.Now(),
	}, nil
}

// addMigratePlaceholder tracks the migrate's new vm as provisioning and
// allocates it on the target, unless a retry already did.
func (fm *FleetManager) addMigratePlaceholder(plan MigratePlan) (*vm, error) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if v, ok := fm.vms[plan.NewVMID]; ok {
		return v, nil
	}
	src, ok := fm.vms[plan.SourceVMID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, plan.SourceVMID)
	}
	spec := src.spec
	spec.Name = plan.NewVMID
	spec.SeedSnapshotID = ""
	spec.PinnedHostID = ""
	spec.HostID = ""
	spec.Labels = nil
	v := &vm{
		id:                plan.NewVMID,
		state:             VMStateProvisioning,
		taskID:            plan.MigrationID,
		hostID:            plan.TargetHostID,
		spec:              spec,
		createdAt:         plan.CreatedAt,
		updatedAt:         plan.CreatedAt,
		lastActivityAt:    plan.CreatedAt,
		hitlKeys:          src.hitlKeys,
		hitlKeysEncrypted: src.hitlKeysEncrypted,
	}
	fm.vms[plan.NewVMID] = v
	if plan.TargetHostID != "" {
		fm.allocateOnHost(plan.TargetHostID, v)
	}
	return v, nil
}

// MigratePlaceholder tracks and persists the migrate's new vm before anything
// is created for it, so neither reconcile nor a restart takes the target for
// an orphan. a restart leaves the row provisioning while restate still runs
// the migrate.
func (fm *FleetManager) MigratePlaceholder(ctx context.Context, plan MigratePlan) error {
	if _, err := fm.addMigratePlaceholder(plan); err != nil {
		return err
	}
	if err := fm.persistVMByID(ctx, plan.NewVMID); err != nil {
		return fmt.Errorf("persist migrate target %s: %w", plan.NewVMID, err)
	}
	return nil
}

// migrateTarget is the migrate's new vm, which the placeholder step tracked.
func (fm *FleetManager) migrateTarget(plan MigratePlan) (*vm, error) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[plan.NewVMID]
	if !ok {
		return nil, fmt.Errorf("%w: migrate target %s is no longer tracked", ErrVMNotFound, plan.NewVMID)
	}
	return v, nil
}

// migrateTargetEnv is the new vm's environment handle. a step after a restart
// finds none in memory and looks the vm up on its host.
func (fm *FleetManager) migrateTargetEnv(ctx context.Context, plan MigratePlan) (*vm, Environment, error) {
	v, err := fm.migrateTarget(plan)
	if err != nil {
		return nil, nil, err
	}
	fm.mu.RLock()
	env := v.env
	fm.mu.RUnlock()
	if env != nil {
		return v, env, nil
	}
	env, err = fm.migrateProvider(plan.TargetHostID).Get(ctx, plan.NewVMID)
	if err != nil {
		return nil, nil, fmt.Errorf("look up migrated vm %s on host %s: %w", plan.NewVMID, plan.TargetHostID, err)
	}
	fm.mu.Lock()
	v.env = env
	fm.mu.Unlock()
	return v, env, nil
}

// MigrateTransferPlan works out how the seed reaches the target: a copy the
// target already holds (Existing), or the move that copies it there.
func (fm *FleetManager) MigrateTransferPlan(ctx context.Context, plan MigratePlan, seed SnapshotRecord) (ChainMove, error) {
	job, existing, err := fm.planArtifactMove(ctx, seed, plan.TargetHostID, plan.Lazy)
	return ChainMove{Job: job, Existing: existing}, err
}

// MigrateDeltaPlan is the move that merges a delta migrate's diff onto the
// copy of its seed on the target, dropping that copy.
func (fm *FleetManager) MigrateDeltaPlan(_ context.Context, plan MigratePlan, child SnapshotRecord, base string) (ArtifactMoveJob, error) {
	return fm.planDeltaMove(child, base, plan.TargetHostID, true)
}

// MigrateCreate creates the new vm on the target: resumed or booted from
// localID, the seed's copy there, or from the seed itself on the same host.
// the host agent's 409 is a resume it refuses (see resume_plan), which leaves
// the source untouched.
func (fm *FleetManager) MigrateCreate(ctx context.Context, plan MigratePlan, seed SnapshotRecord, localID string) error {
	v, err := fm.migrateTarget(plan)
	if err != nil {
		return err
	}
	var env Environment
	if plan.SameHost() {
		forkable, ok := fm.migrateProvider(plan.SourceHostID).(SnapshotForkable)
		if !ok {
			return fmt.Errorf("provider does not support migrate for vm %s", plan.SourceVMID)
		}
		fm.mu.RLock()
		spec := v.spec
		fm.mu.RUnlock()
		env, err = forkable.CreateFromCheckpoint(ctx, spec, plan.SourceVMID, seed.SnapshotID)
	} else {
		fm.mu.Lock()
		v.spec.SeedSnapshotID = localID
		v.spec.PinnedHostID = plan.TargetHostID
		v.spec.ResumeSeed = plan.Live
		spec := v.spec
		fm.mu.Unlock()
		fm.touchArtifact(localID, seed.SnapshotID)
		env, err = fm.migrateProvider(plan.TargetHostID).Create(ctx, spec)
	}
	if err != nil {
		var httpErr *HTTPStatusError
		if plan.Live && errors.As(err, &httpErr) && httpErr.Code == http.StatusConflict {
			return fmt.Errorf("%w: host %s cannot resume vm %s: %w", ErrLiveMigrateRefused, plan.TargetHostID, plan.SourceVMID, err)
		}
		return fmt.Errorf("migrate vm %s from snapshot %s: %w", plan.SourceVMID, seed.SnapshotID, err)
	}
	fm.mu.Lock()
	v.env = env
	fm.mu.Unlock()
	return nil
}

// sealedIngress is an ingressPlan as MigrateStarted carries it.
type sealedIngress struct {
	TunnelConfig []byte         `json:"tunnel_config"`
	Grant        IngressGrant   `json:"grant"`
	Request      IngressRequest `json:"request"`
}

// sealIngress seals p under the token encryption key: its tunnel token lets a
// guest dial the proxy as the vm. a fleet with no key runs its guests without
// credentials at all, and carries it as is.
func (fm *FleetManager) sealIngress(p *ingressPlan) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	raw, err := json.Marshal(sealedIngress{TunnelConfig: p.tunnelConfig, Grant: p.grant, Request: p.request})
	if err != nil {
		return nil, err
	}
	if len(fm.tokenEncryptionKey) != 32 {
		return raw, nil
	}
	return secrets.EncryptToken(string(raw), fm.tokenEncryptionKey)
}

// openIngress is sealIngress undone.
func (fm *FleetManager) openIngress(sealed []byte) (*ingressPlan, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	raw := sealed
	if len(fm.tokenEncryptionKey) == 32 {
		plain, err := secrets.DecryptToken(sealed, fm.tokenEncryptionKey)
		if err != nil {
			return nil, fmt.Errorf("open migrate ingress: %w", err)
		}
		raw = []byte(plain)
	}
	var s sealedIngress
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("open migrate ingress: %w", err)
	}
	return &ingressPlan{tunnelConfig: s.TunnelConfig, grant: s.Grant, request: s.Request}, nil
}

// MigrateIngress publishes the new vm under ports of its own. the source's
// ports are adopted at the cutover, once nothing can fail any more. a retry
// publishes the same owner again.
func (fm *FleetManager) MigrateIngress(ctx context.Context, plan MigratePlan) ([]byte, error) {
	v, err := fm.migrateTarget(plan)
	if err != nil {
		return nil, err
	}
	fm.mu.RLock()
	spec := v.spec
	fm.mu.RUnlock()
	if !fm.ingressSupported(plan.TargetHostID, spec) {
		return nil, nil
	}
	p, err := fm.publishIngress(ctx, plan.NewVMID, nil, "")
	if err != nil {
		return nil, err
	}
	return fm.sealIngress(p)
}

// MigrateCredentials makes the new vm credentials of its own, returned
// sealed. it uploads nothing: a durable retry of the upload has to send the
// same files, so they are made once, in a step of their own. a fleet with no
// encryption key gives the vm none.
func (fm *FleetManager) MigrateCredentials(_ context.Context, plan MigratePlan) (RotatedCredentials, error) {
	if len(fm.tokenEncryptionKey) != 32 {
		return RotatedCredentials{}, nil
	}
	creds, err := secrets.GenerateVMCredentials(plan.NewVMID)
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("generate credentials for migrated vm %s: %w", plan.NewVMID, err)
	}
	raw, err := json.Marshal(sealedCredentials{AuthToken: creds.AuthToken, CertPEM: creds.CertPEM, KeyPEM: creds.KeyPEM})
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("encode credentials for migrated vm %s: %w", plan.NewVMID, err)
	}
	sealed, err := secrets.EncryptBytes(raw, fm.tokenEncryptionKey)
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("encrypt credentials for migrated vm %s: %w", plan.NewVMID, err)
	}
	token, err := secrets.EncryptToken(creds.AuthToken, fm.tokenEncryptionKey)
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("encrypt token for migrated vm %s: %w", plan.NewVMID, err)
	}
	return RotatedCredentials{Sealed: sealed, TokenEncrypted: token}, nil
}

// MigrateUpload writes the new vm's tunnel file and credential files into the
// guest. one at a time in path order, not concurrently like uploadFiles: on
// restate each upload is keyed by its place in the step, and the agent
// replays a key's first answer whatever file it now carries, so a retried
// step has to send the same file under each key.
func (fm *FleetManager) MigrateUpload(ctx context.Context, plan MigratePlan, started MigrateStarted) error {
	_, env, err := fm.migrateTargetEnv(ctx, plan)
	if err != nil {
		return err
	}
	ip, err := fm.openIngress(started.Ingress)
	if err != nil {
		return err
	}
	files := ip.tunnelFiles()
	var creds *secrets.VMCredentials
	if len(started.Credentials.Sealed) > 0 {
		raw, err := secrets.DecryptBytes(started.Credentials.Sealed, fm.tokenEncryptionKey)
		if err != nil {
			return fmt.Errorf("open credentials for migrated vm %s: %w", plan.NewVMID, err)
		}
		var sealed sealedCredentials
		if err := json.Unmarshal(raw, &sealed); err != nil {
			return fmt.Errorf("open credentials for migrated vm %s: %w", plan.NewVMID, err)
		}
		creds = &secrets.VMCredentials{AuthToken: sealed.AuthToken, CertPEM: sealed.CertPEM, KeyPEM: sealed.KeyPEM}
		files = ip.withTunnelFiles(fusedCredentialFiles(creds))
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err := env.Upload(ctx, files[path], path); err != nil {
			return fmt.Errorf("upload credentials to migrated vm %s: upload %s: %w", plan.NewVMID, path, err)
		}
	}
	setTokenIfSupported(env, creds)
	return nil
}

// migrateToken is the new vm's plaintext token, and hands it to env.
func (fm *FleetManager) migrateToken(env Environment, started MigrateStarted) (string, error) {
	enc := started.Credentials.TokenEncrypted
	if len(enc) == 0 || len(fm.tokenEncryptionKey) != 32 {
		return "", nil
	}
	token, err := secrets.DecryptToken(enc, fm.tokenEncryptionKey)
	if err != nil {
		return "", fmt.Errorf("decrypt migrated vm token: %w", err)
	}
	if ts, ok := env.(TokenSetter); ok {
		ts.SetToken(token)
	}
	return token, nil
}

// MigrateStartAgent restarts the guest agent on the new vm with its own
// credentials and tunnel. on a fleet with no encryption key it only restarts
// it onto the vm's own tunnel (see retunnelWithoutCredentials), and leaves an
// unpublished vm's agent alone.
func (fm *FleetManager) MigrateStartAgent(ctx context.Context, plan MigratePlan, started MigrateStarted) error {
	_, env, err := fm.migrateTargetEnv(ctx, plan)
	if err != nil {
		return err
	}
	ip, err := fm.openIngress(started.Ingress)
	if err != nil {
		return err
	}
	if len(fm.tokenEncryptionKey) != 32 {
		if ip == nil {
			return nil
		}
		if err := env.StartAgent(ctx, AgentSpec{DrainCommand: DefaultFusedDrainCommand, TunnelConfigPath: ip.tunnelConfigPath()}); err != nil {
			return fmt.Errorf("start tunnel on migrated vm %s: %w", plan.NewVMID, err)
		}
		return nil
	}
	token, err := fm.migrateToken(env, started)
	if err != nil {
		return err
	}
	if err := env.StartAgent(ctx, AgentSpec{
		AuthToken:        token,
		DrainCommand:     DefaultFusedDrainCommand,
		TunnelConfigPath: ip.tunnelConfigPath(),
	}); err != nil {
		return fmt.Errorf("restart guest agent on migrated vm %s with its own credentials: %w", plan.NewVMID, err)
	}
	return nil
}

// MigratePersist records the new vm running. from here it is the guest.
func (fm *FleetManager) MigratePersist(ctx context.Context, plan MigratePlan, started MigrateStarted) error {
	v, env, err := fm.migrateTargetEnv(ctx, plan)
	if err != nil {
		return err
	}
	if _, err := fm.migrateToken(env, started); err != nil {
		return err
	}
	fm.mu.Lock()
	prevState := v.state
	v.state = VMStateRunning
	v.env = env
	v.url = env.URL()
	v.authTokenEncrypted = started.Credentials.TokenEncrypted
	v.drainCommand = DefaultFusedDrainCommand
	v.updatedAt = time.Now()
	fm.mu.Unlock()
	if err := fm.persistVMByID(ctx, plan.NewVMID); err != nil {
		// a retry still sees a vm that is not running yet.
		fm.mu.Lock()
		v.state = prevState
		fm.mu.Unlock()
		return fmt.Errorf("persist migrated vm %s running state: %w", plan.NewVMID, err)
	}
	return nil
}

// MigrateTask records the new vm's task running. a failure is logged: the vm
// itself is recorded already.
func (fm *FleetManager) MigrateTask(ctx context.Context, plan MigratePlan) error {
	if fm.store == nil {
		return nil
	}
	if err := fm.store.UpsertTask(ctx, TaskRecord{
		TaskID:     plan.MigrationID,
		VMID:       plan.NewVMID,
		RunStatus:  TaskRunRunning,
		AssignedAt: plan.CreatedAt,
		UpdatedAt:  plan.CreatedAt,
	}); err != nil {
		fm.logger.WarnContext(ctx, "persist migrated task running state failed", "vm", plan.NewVMID, "task", plan.MigrationID, "err", err)
	}
	return nil
}

// MigrateCutover moves the source's public ports onto the new vm: from here
// a client of the source's url reaches it. a failure is logged, and the new
// vm keeps a url of its own.
func (fm *FleetManager) MigrateCutover(ctx context.Context, plan MigratePlan, started MigrateStarted) error {
	ip, err := fm.openIngress(started.Ingress)
	if err != nil {
		fm.logger.WarnContext(ctx, "adopt ingress skipped; the migrated vm keeps a new url", "vm", plan.NewVMID, "err", err)
		ip = nil
	}
	fm.adoptIngress(ctx, ip, plan.SourceVMID)
	if ip != nil {
		fm.mu.Lock()
		if v, ok := fm.vms[plan.NewVMID]; ok {
			v.applyIngress(ip.grant)
		}
		fm.mu.Unlock()
	}
	fm.publishStateChange(plan.NewVMID, "")
	return nil
}

// MigrateLineage records the new vm's descent from the seed and the
// vm.migrated event. a failed record is logged.
func (fm *FleetManager) MigrateLineage(ctx context.Context, plan MigratePlan, seed SnapshotRecord, delta bool) error {
	lineageMeta, _ := marshalSnapshotMetadata("", nil)
	lineage := SnapshotRecord{
		SnapshotID:       "migrate-seed-" + plan.MigrationID,
		VMID:             plan.NewVMID,
		TaskID:           plan.MigrationID,
		HostID:           plan.TargetHostID,
		TenantID:         snapshotTenantID(plan.MigrationID, plan.NewVMID),
		ParentSnapshotID: seed.SnapshotID,
		Mode:             SnapshotModeAuto,
		State:            SnapshotStateReady,
		Metadata:         lineageMeta,
		CreatedAt:        plan.CreatedAt,
		UpdatedAt:        plan.CreatedAt,
	}
	if err := fm.upsertSnapshotRecord(ctx, lineage); err != nil {
		fm.logger.WarnContext(ctx, "persist migrate lineage record failed", "vm", plan.NewVMID, "parent", seed.SnapshotID, "err", err)
	}
	fm.appendEvent(ctx, "vm", plan.NewVMID, "vm.migrated", map[string]any{
		"source_vm_id":     plan.SourceVMID,
		"source_host_id":   plan.SourceHostID,
		"target_host_id":   plan.TargetHostID,
		"seed_snapshot_id": seed.SnapshotID,
		"live":             plan.Live,
		"lazy":             plan.Lazy,
		"delta":            delta,
	})
	return nil
}

// MigrateHandover moves what belongs to the logical environment onto the new
// vm: pending decisions, grants, a held takeover and scoped keys. it runs
// before the source is destroyed, which would otherwise cancel them.
func (fm *FleetManager) MigrateHandover(ctx context.Context, plan MigratePlan) error {
	fm.moveInterventions(ctx, plan.SourceVMID, plan.NewVMID)
	fm.rebindEnvKeys(ctx, plan.SourceVMID, plan.NewVMID)
	return nil
}

// MigrateDrainSource runs the source's drain command before it is destroyed.
// a failure is logged: the source is destroyed anyway. it drains in process,
// never through restate: on restate this step runs on the source's own
// Environment object, which a drain sent there would queue behind.
func (fm *FleetManager) MigrateDrainSource(ctx context.Context, plan MigratePlan) error {
	if err := fm.drainInProcess(ctx, plan.SourceVMID); err != nil {
		fm.logger.WarnContext(ctx, "drain source vm during migration failed; destroying anyway", "vm", plan.SourceVMID, "err", err)
	}
	return nil
}

// MigrateResumeSource resumes a source a failed delta migrate left paused:
// until the target runs it is the only copy of the guest. a source that is
// gone, or that the host refuses to resume because it is not paused, needs
// nothing.
func (fm *FleetManager) MigrateResumeSource(ctx context.Context, plan MigratePlan) error {
	fm.mu.RLock()
	v, ok := fm.vms[plan.SourceVMID]
	var env Environment
	if ok {
		env = v.env
	}
	fm.mu.RUnlock()
	if env == nil {
		var err error
		env, err = fm.migrateProvider(plan.SourceHostID).Get(ctx, plan.SourceVMID)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up source vm %s to resume it: %w", plan.SourceVMID, err)
		}
	}
	d, ok := env.(DeltaSnapshotCapable)
	if !ok {
		return nil
	}
	err := d.Resume(ctx)
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) && httpErr.Code >= 400 && httpErr.Code < 500 {
		fm.logger.WarnContext(ctx, "resume source vm after a failed delta migrate refused; taking it as running", "vm", plan.SourceVMID, "err", err)
		return nil
	}
	if err != nil {
		fm.logger.ErrorContext(ctx, "resume source vm after a failed delta migrate", "vm", plan.SourceVMID, "err", err)
		return err
	}
	return nil
}

// MigrateFrozen records how long a delta migrate kept its source paused.
func (fm *FleetManager) MigrateFrozen(ctx context.Context, plan MigratePlan, d time.Duration, outcome string) error {
	if m, ok := fm.metrics.(MigrateMetrics); ok {
		m.MigrateFrozen(outcome, d)
	}
	fm.logger.InfoContext(ctx, "migrate source frozen", "vm", plan.SourceVMID, "seconds", d.Seconds(), "outcome", outcome)
	return nil
}

// MigrateDeleteSeed deletes the seed a failed migrate took. failures are
// logged and left to retention.
func (fm *FleetManager) MigrateDeleteSeed(ctx context.Context, seed SnapshotRecord) error {
	fm.deleteCheckpoint(ctx, seed)
	return nil
}

// MigrateMemoryPoll asks a lazily resumed vm's host how far its memory
// transfer is. an error is transient (the agent may be restarting) and reads
// as no news.
func (fm *FleetManager) MigrateMemoryPoll(ctx context.Context, vmID string) (MemoryPoll, error) {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	hostID := ""
	if ok {
		hostID = v.hostID
	}
	fm.mu.RUnlock()
	if !ok {
		return MemoryPoll{Gone: true}, nil
	}
	w, ok := fm.migrateProvider(hostID).(MemoryWatcher)
	if !ok {
		return MemoryPoll{Gone: true}, nil
	}
	st, err := w.MemoryStatus(ctx, vmID)
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) && httpErr.Code == http.StatusNotFound {
		// the vm was destroyed before its memory finished arriving.
		return MemoryPoll{Gone: true}, nil
	}
	if err != nil {
		return MemoryPoll{}, nil
	}
	return MemoryPoll{Status: st}, nil
}

// MigrateMemorySettled records how a lazy migrate's memory transfer ended:
// resident, failed, or timed out. a failed transfer leaves a guest that will
// hang on its next missing page, and the only thing the orchestrator can do
// is say so. a vm that is gone needs no event.
func (fm *FleetManager) MigrateMemorySettled(ctx context.Context, vmID, seedID string, poll MemoryPoll, timedOut bool) error {
	switch {
	case poll.Gone:
	case poll.Status.Done:
		fm.appendEvent(ctx, "vm", vmID, "vm.memory_resident", map[string]any{
			"seed_snapshot_id": seedID,
			"chunks":           poll.Status.TotalChunks,
		})
	case poll.Status.Error != "":
		fm.appendEvent(ctx, "vm", vmID, "vm.memory_transfer_failed", map[string]any{
			"seed_snapshot_id": seedID,
			"error":            poll.Status.Error,
		})
	case timedOut:
		fm.appendEvent(ctx, "vm", vmID, "vm.memory_transfer_failed", map[string]any{
			"seed_snapshot_id": seedID,
			"error":            "timed out waiting for the memory image",
		})
	}
	return nil
}
