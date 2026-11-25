package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/folsomintel/fuse/internal/egress"
	"github.com/folsomintel/fuse/internal/secrets"
)

// opCreate is the op label of a create, on its operation metric and on the
// metrics and spans of its steps.
const opCreate = "create"

// errCreateSealed is a create whose sealed inputs no longer decrypt, as after
// TOKEN_ENCRYPTION_KEY changed under an unfinished create.
var errCreateSealed = errors.New("create inputs do not decrypt under TOKEN_ENCRYPTION_KEY")

// CreateFleetErrors are the sentinel errors a create step fails with when a
// retry cannot change the outcome. the durable handler carries each one
// across restate as its own error code, so the api answers it with the
// status the sentinel maps to.
var CreateFleetErrors = []error{
	ErrTaskAlreadyAssigned,
	ErrNoCapacity,
	ErrNoHosts,
	ErrHostPinUnschedulable,
	ErrNoLabelMatch,
	ErrStartupScriptTimeout,
	ErrHitlNotDeclared,
	ErrHitlSecretMissing,
	// a code is its index, and create has ten (530 to 539). this slot was
	// ErrHitlNeedsKey, which no durable create could fail with (it always
	// had the key, which is now required), so no journal holds its code.
	ErrImageNotReady,
	ErrVMNotFound,
}

// PermanentCreateError reports whether a create step failed in a way a retry
// cannot change: one of CreateFleetErrors, a failure the step marked
// permanent, inputs that do not decrypt, or an agent that refused the request
// with a 4xx other than a timeout or rate limit.
func PermanentCreateError(err error) bool {
	if err == nil {
		return false
	}
	for _, s := range CreateFleetErrors {
		if errors.Is(err, s) {
			return true
		}
	}
	var marked permanentCreateError
	if errors.As(err, &marked) || errors.Is(err, errCreateSealed) {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		code := statusErr.Code
		return code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
	}
	return false
}

// permanentCreateError marks a create step failure a retry cannot change
// that is not one of CreateFleetErrors: a scheduling or artifact failure, a
// startup script that exited non-zero, an egress backend that refused. its
// message is the cause's.
type permanentCreateError struct{ error }

func (e permanentCreateError) Unwrap() error { return e.error }

// createPermanentErr marks err permanent.
func createPermanentErr(err error) error {
	if err == nil || PermanentCreateError(err) {
		return err
	}
	return permanentCreateError{err}
}

// redactedError is a create step failure as the caller sees it, with the
// request's secret values redacted from its message, since it is journaled
// and logged. errors.Is and errors.As still see the cause.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// sensitive is every value of the run an error string must not carry: the
// caller's secrets, the gateway url and token (a url can embed credentials,
// and the host agent echoes its launch command into a timeout error) and the
// vm's auth token.
func (r *createRun) sensitive() map[string]string {
	out := make(map[string]string, len(r.in.Secrets)+3)
	for k, v := range r.in.Secrets {
		out[k] = v
	}
	out["\x00gateway_url"] = r.in.GatewayURL
	out["\x00gateway_token"] = r.in.GatewayToken
	if r.creds != nil {
		out["\x00auth_token"] = r.creds.AuthToken
	}
	return out
}

// provisionErr names err after the vm and redacts it.
func provisionErr(vmID string, err error, secretMap map[string]string) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: secrets.RedactSecretValues(fmt.Sprintf("provision vm %s: %v", vmID, err), secretMap), cause: err}
}

// CreateRequest is the input of the durable Environment/{vm_id}.Create
// handler. restate journals it, so everything a caller can put a secret in
// (the spec, whose hitl webhook urls can carry a token, the manifest,
// secrets, startup script, gateway token, files) is sealed.
type CreateRequest struct {
	TaskID string `json:"task_id"`
	// Sealed is createInputs as json, encrypted under TOKEN_ENCRYPTION_KEY.
	Sealed []byte `json:"sealed"`
}

// createInputs is the plaintext inside CreateRequest.Sealed: what
// ProvisionAndAssign takes besides the task id and spec.
type createInputs struct {
	Spec                 Spec              `json:"spec"`
	Manifest             []byte            `json:"manifest,omitempty"`
	Secrets              map[string]string `json:"secrets,omitempty"`
	StartupScript        string            `json:"startup_script,omitempty"`
	StartupScriptTimeout time.Duration     `json:"startup_script_timeout,omitempty"`
	GatewayURL           string            `json:"gateway_url,omitempty"`
	GatewayToken         string            `json:"gateway_token,omitempty"`
	Expose               []ExposeSpec      `json:"expose,omitempty"`
	Healthcheck          *HealthcheckSpec  `json:"healthcheck,omitempty"`
	Desktop              *DesktopSpec      `json:"desktop,omitempty"`
	Files                map[string][]byte `json:"files,omitempty"`
}

// CreateState is what the create's steps hand on to the ones after them. the
// handler journals each part as the step that made it returns it, so a
// replay rebuilds the same state. secret parts are ciphertext.
type CreateState struct {
	// Credentials is the vm's tls cert, key and auth token as json, sealed.
	Credentials []byte `json:"credentials,omitempty"`
	// HostID is where placement put the vm, "" on a fleet with no hosts.
	HostID string `json:"host_id,omitempty"`
	// SeedSnapshotID is the seed artifact's copy on the vm's host, "" when
	// the vm is not seeded.
	SeedSnapshotID string `json:"seed_snapshot_id,omitempty"`
	// Ingress is the vm's fuse-proxy publication, nil when it has none.
	Ingress *CreateIngress `json:"ingress,omitempty"`
	// Booted is set once the boot step has started: from then on a vm may
	// exist on the host, and a rollback destroys it.
	Booted bool `json:"booted,omitempty"`
	// FromCache reports a vm restored from its own checkpoint.
	FromCache bool `json:"from_cache,omitempty"`
	// Endpoints is what the guest agent published as it started.
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// Epoch is the vm's incarnation, kept so the cleanup destroy of a vm
	// the abandon dropped is fenced to it.
	Epoch int64 `json:"epoch,omitempty"`
}

// CreateIngress is a published vm: the sidecar config its guest needs,
// sealed because it carries the tunnel token, and the proxy's answer.
type CreateIngress struct {
	TunnelConfig []byte       `json:"tunnel_config"`
	Grant        IngressGrant `json:"grant"`
}

// DurableCreator runs a create as the restate Environment/{vm_id}.Create
// handler and waits for it. the create keeps running if ctx ends first.
// *durable.Client satisfies it.
type DurableCreator interface {
	Create(ctx context.Context, vmID string, req CreateRequest) error
	// CreateInFlight reports whether restate holds an unfinished create of
	// vmID.
	CreateInFlight(ctx context.Context, vmID string) (bool, error)
}

// SetDurableCreator routes creates through restate. creates seal their
// inputs with the fleet's TOKEN_ENCRYPTION_KEY before restate journals them.
// set it before Start.
func (fm *FleetManager) SetDurableCreator(d DurableCreator) {
	fm.mu.Lock()
	fm.durableCreate = d
	fm.mu.Unlock()
}

// durableCreator is the creator creates go through.
func (fm *FleetManager) durableCreator() DurableCreator {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableCreate
}

// durableProvision is provisionAndAssign on restate: it seals the inputs,
// runs the handler and waits for it, then reads the vm the handler left
// running.
func (fm *FleetManager) durableProvision(ctx context.Context, d DurableCreator, taskID string, spec Spec, manifest []byte, secretMap map[string]string, opts BootOptions) (*VMInfo, error) {
	vmID := fm.prefix + taskID
	spec.Name = vmID
	// a retry carrying an idempotency key attaches to the invocation the
	// first request started, which owns the vm this check would trip on.
	if IdempotencyKey(ctx) == "" {
		if err := fm.taskUnassigned(taskID, ""); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(createInputs{
		Spec:                 spec,
		Manifest:             manifest,
		Secrets:              secretMap,
		StartupScript:        opts.StartupScript,
		StartupScriptTimeout: opts.StartupScriptTimeout,
		GatewayURL:           opts.GatewayURL,
		GatewayToken:         opts.GatewayToken,
		Expose:               opts.Expose,
		Healthcheck:          opts.Healthcheck,
		Desktop:              opts.Desktop,
		Files:                opts.Files,
	})
	if err != nil {
		return nil, fmt.Errorf("encode create inputs: %w", err)
	}
	sealed, err := secrets.EncryptBytes(raw, fm.tokenEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("seal create inputs: %w", err)
	}
	ctx, cancel := durableWait(ctx)
	defer cancel()
	if err := d.Create(ctx, vmID, CreateRequest{TaskID: taskID, Sealed: sealed}); err != nil {
		return nil, err
	}
	info, ok := fm.GetVM(vmID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	return &info, nil
}

// taskUnassigned fails with ErrTaskAlreadyAssigned when a vm other than
// owner holds taskID.
func (fm *FleetManager) taskUnassigned(taskID, owner string) error {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	for _, v := range fm.vms {
		if v.taskID == taskID && v.id != owner {
			return fmt.Errorf("%w: task %s already assigned to vm %s", ErrTaskAlreadyAssigned, taskID, v.id)
		}
	}
	return nil
}

// createRun is what a create's steps keep in memory between them: the
// decrypted inputs and the environment handle. it saves every step from
// decrypting the request and looking the vm up on its host again. after a
// restart it is rebuilt from the request, the journaled state and the vm.
type createRun struct {
	sealed    []byte
	in        createInputs
	creds     *secrets.VMCredentials
	agentSpec *AgentSpec
	env       Environment
}

// createRunFor returns vmID's run for req, decrypting req and st's
// credentials when this process has none for it.
func (fm *FleetManager) createRunFor(vmID string, req CreateRequest, st CreateState) (*createRun, error) {
	fm.mu.RLock()
	run, ok := fm.createRuns[vmID]
	fm.mu.RUnlock()
	// the ciphertext is unique per request, so a run an earlier create of the
	// same vm left behind is never taken for this one.
	if ok && bytes.Equal(run.sealed, req.Sealed) && (run.creds != nil || len(st.Credentials) == 0) {
		return run, nil
	}
	raw, err := secrets.DecryptBytes(req.Sealed, fm.tokenEncryptionKey)
	if err != nil {
		return nil, errCreateSealed
	}
	run = &createRun{sealed: req.Sealed}
	if err := json.Unmarshal(raw, &run.in); err != nil {
		return nil, errCreateSealed
	}
	if len(st.Credentials) > 0 {
		if run.creds, err = fm.openCredentials(st.Credentials); err != nil {
			return nil, err
		}
	}
	fm.mu.Lock()
	fm.createRuns[vmID] = run
	fm.mu.Unlock()
	return run, nil
}

// dropCreateRun forgets vmID's run once its create has ended.
func (fm *FleetManager) dropCreateRun(vmID string) {
	fm.mu.Lock()
	delete(fm.createRuns, vmID)
	fm.mu.Unlock()
}

// openCredentials decrypts credentials CreateBegin sealed.
func (fm *FleetManager) openCredentials(sealed []byte) (*secrets.VMCredentials, error) {
	raw, err := secrets.DecryptBytes(sealed, fm.tokenEncryptionKey)
	if err != nil {
		return nil, errCreateSealed
	}
	var sc sealedCredentials
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, errCreateSealed
	}
	return &secrets.VMCredentials{AuthToken: sc.AuthToken, CertPEM: sc.CertPEM, KeyPEM: sc.KeyPEM}, nil
}

// createVM returns the tracked vm a create is building. ErrVMNotFound means
// it is gone, which a retry cannot change.
func (fm *FleetManager) createVM(vmID string) (*vm, error) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[vmID]
	if !ok || v.state != VMStateProvisioning {
		return nil, fmt.Errorf("%w: %s is not being created", ErrVMNotFound, vmID)
	}
	return v, nil
}

// createSpec is the spec the create's steps work from: the one the caller
// asked for, from the sealed request, with what earlier steps resolved. the
// vm row keeps only part of a spec (no image, seed or egress secret), so a
// create resumed after a restart must not read it back from there. caller
// holds fm.mu.
func createSpec(v *vm, run *createRun, st CreateState) Spec {
	spec := run.in.Spec
	spec.Name = v.id
	// placement binds gpu devices onto the tracked spec, which the row keeps.
	spec.GPUUUIDs = v.spec.GPUUUIDs
	spec.MIGInstanceUUIDs = v.spec.MIGInstanceUUIDs
	spec.SharedMemSeed = v.spec.SharedMemSeed
	if st.SeedSnapshotID != "" {
		spec.SeedSnapshotID = st.SeedSnapshotID
		spec.PinnedHostID = st.HostID
	}
	return spec
}

// createBoot returns the boot the steps after placement drive: the vm's
// provider, inputs and environment handle. needEnv looks the vm up on its
// host when this process has no handle for it, as after a restart.
func (fm *FleetManager) createBoot(ctx context.Context, vmID string, req CreateRequest, st CreateState, needEnv bool) (*bootRun, *vm, *createRun, error) {
	v, err := fm.createVM(vmID)
	if err != nil {
		return nil, nil, nil, err
	}
	run, err := fm.createRunFor(vmID, req, st)
	if err != nil {
		return nil, nil, nil, err
	}
	p, err := fm.providerForVM(st.HostID)
	if err != nil {
		return nil, nil, nil, err
	}
	fm.mu.RLock()
	spec := createSpec(v, run, st)
	status := v.egress
	fm.mu.RUnlock()

	if run.agentSpec == nil {
		var tunnel []byte
		if st.Ingress != nil {
			if tunnel, err = secrets.DecryptBytes(st.Ingress.TunnelConfig, fm.tokenEncryptionKey); err != nil {
				return nil, nil, nil, errCreateSealed
			}
		}
		agentSpec := FusedAgentSpec(run.in.Manifest, GuestSecrets(run.in.Secrets, spec.Egress), run.creds, run.bootOptions(tunnel))
		run.agentSpec = &agentSpec
	}
	opts := run.bootOptions(nil)
	opts.HostID = st.HostID
	opts.EgressRegistry = fm.egressRegistry
	b := &bootRun{
		p:         p,
		in:        bootInputs{spec: spec, manifest: run.in.Manifest, secretMap: run.in.Secrets, creds: run.creds, agentSpec: *run.agentSpec, opts: opts},
		env:       run.env,
		fromCache: st.FromCache,
		egress:    status,
	}
	if needEnv && b.env == nil {
		env, err := p.Get(fm.epochCtx(ctx, vmID), vmID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("look up vm %s on its host: %w", vmID, err)
		}
		setTokenIfSupported(env, run.creds)
		run.env, b.env = env, env
	}
	return b, v, run, nil
}

// bootOptions is the run's inputs as the BootOptions FusedAgentSpec reads.
func (r *createRun) bootOptions(tunnel []byte) BootOptions {
	return BootOptions{
		StartupScript:        r.in.StartupScript,
		StartupScriptTimeout: r.in.StartupScriptTimeout,
		GatewayURL:           r.in.GatewayURL,
		GatewayToken:         r.in.GatewayToken,
		Expose:               r.in.Expose,
		Healthcheck:          r.in.Healthcheck,
		Desktop:              r.in.Desktop,
		TunnelConfig:         tunnel,
		Files:                r.in.Files,
	}
}

// the methods below are the steps of the durable create, in order, and then
// its compensation. each runs inside a journaled restate step and must be
// safe to run again: a crash after a step's effect and before its journal
// entry replays it. host-agent calls inside a step are keyed on the step, so
// a replayed create, exec or start-agent gets the agent's first answer back.

// CreateBegin checks the task is free, generates the vm's credentials and
// records the vm provisioning and its task assigned. a vm left provisioning
// by an earlier run of this step is taken over: restate runs one create per
// vm at a time, so it can only be this create's.
func (fm *FleetManager) CreateBegin(ctx context.Context, vmID string, req CreateRequest) (CreateState, error) {
	if err := fm.taskUnassigned(req.TaskID, vmID); err != nil {
		return CreateState{}, err
	}
	run, err := fm.createRunFor(vmID, req, CreateState{})
	if err != nil {
		return CreateState{}, err
	}
	hitlKeys, hitlKeysEncrypted, err := fm.hitlKeysFor(run.in.Spec.Hitl, run.in.Secrets)
	if err != nil {
		return CreateState{}, err
	}
	creds, err := secrets.GenerateVMCredentials(vmID)
	if err != nil {
		return CreateState{}, fmt.Errorf("generate vm credentials: %w", err)
	}
	rawCreds, err := json.Marshal(sealedCredentials{AuthToken: creds.AuthToken, CertPEM: creds.CertPEM, KeyPEM: creds.KeyPEM})
	if err != nil {
		return CreateState{}, fmt.Errorf("encode vm credentials: %w", err)
	}
	sealedCreds, err := secrets.EncryptBytes(rawCreds, fm.tokenEncryptionKey)
	if err != nil {
		return CreateState{}, fmt.Errorf("seal vm credentials: %w", err)
	}
	encToken, err := secrets.EncryptToken(creds.AuthToken, fm.tokenEncryptionKey)
	if err != nil {
		return CreateState{}, fmt.Errorf("encrypt vm token: %w", err)
	}

	now := time.Now()
	spec := run.in.Spec
	spec.Name = vmID
	fm.mu.Lock()
	v, again := fm.vms[vmID]
	if again && (v.state != VMStateProvisioning || v.taskID != req.TaskID) {
		fm.mu.Unlock()
		return CreateState{}, fmt.Errorf("%w: task %s already assigned to vm %s", ErrTaskAlreadyAssigned, req.TaskID, vmID)
	}
	if !again {
		v = &vm{id: vmID, state: VMStateProvisioning, taskID: req.TaskID, createdAt: now, lastActivityAt: now}
		fm.vms[vmID] = v
	}
	v.spec = spec
	v.updatedAt = now
	v.healthcheck = run.in.Healthcheck
	v.hitlKeys, v.hitlKeysEncrypted = hitlKeys, hitlKeysEncrypted
	v.authTokenEncrypted = encToken
	createdAt := v.createdAt
	fm.mu.Unlock()
	run.creds = creds
	run.agentSpec = nil

	drop := func() {
		if again {
			return
		}
		fm.mu.Lock()
		delete(fm.vms, vmID)
		fm.mu.Unlock()
	}
	if err := fm.persistVM(ctx, v); err != nil {
		drop()
		return CreateState{}, fmt.Errorf("persist vm %s provisioning state: %w", vmID, err)
	}
	if err := fm.store.UpsertTask(ctx, TaskRecord{
		TaskID:     req.TaskID,
		VMID:       vmID,
		RunStatus:  TaskRunAssigned,
		AssignedAt: createdAt,
		UpdatedAt:  now,
	}); err != nil {
		drop()
		if !again {
			_ = fm.store.DeleteVM(ctx, vmID)
		}
		return CreateState{}, fmt.Errorf("persist task %s assigned state: %w", req.TaskID, err)
	}
	if !again {
		fm.appendEvent(ctx, "vm", vmID, "vm.provisioning", map[string]any{"task_id": req.TaskID})
		fm.appendEvent(ctx, "task", req.TaskID, "task.assigned", map[string]any{"vm_id": vmID})
		fm.publishStateChange(vmID, "")
	}
	fm.logger.InfoContext(ctx, "provisioning vm", "vm", vmID, "task", req.TaskID)
	return CreateState{Credentials: sealedCreds, Epoch: VMEpoch(createdAt)}, nil
}

// CreateBeginFailed cleans up after a begin that failed for good: a vm
// provisioning for this task that an earlier attempt of begin recorded is
// abandoned. anything else under the vm id, such as the running vm of a
// task already assigned, is left alone.
func (fm *FleetManager) CreateBeginFailed(ctx context.Context, vmID string, req CreateRequest, cause string) error {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	ours := ok && v.state == VMStateProvisioning && v.taskID == req.TaskID
	fm.mu.RUnlock()
	if !ours {
		fm.dropCreateRun(vmID)
		return nil
	}
	return fm.CreateAbandon(ctx, vmID, req, cause)
}

// CreatePlace schedules the vm and reserves its capacity, and persists the
// placement so a restart re-derives the reservation. a vm already placed by
// an earlier run of this step keeps its host. every scheduling failure is
// permanent: retrying cannot make a host fit.
func (fm *FleetManager) CreatePlace(ctx context.Context, vmID string, req CreateRequest, st CreateState) (string, error) {
	v, err := fm.createVM(vmID)
	if err != nil {
		return "", err
	}
	run, err := fm.createRunFor(vmID, req, st)
	if err != nil {
		return "", err
	}
	fm.mu.Lock()
	hostID := v.hostID
	if hostID == "" {
		// schedule on the spec the caller asked for, not the partial one a
		// restart read back from the row.
		v.spec = createSpec(v, run, st)
	}
	spec := v.spec
	fm.mu.Unlock()
	if hostID != "" {
		return hostID, nil
	}
	if _, err := fm.placeVM(ctx, v, fm.planSeedPlacement(ctx, spec)); err != nil {
		return "", createPermanentErr(err)
	}
	if err := fm.persistVM(ctx, v); err != nil {
		fm.mu.Lock()
		if v.hostID != "" {
			fm.deallocateOnHost(v.hostID, v.spec)
			v.hostID = ""
		}
		fm.mu.Unlock()
		return "", fmt.Errorf("persist vm %s placement: %w", vmID, err)
	}
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return v.hostID, nil
}

// CreateArtifact copies the seed artifact to the vm's host when placement put
// it somewhere that does not hold it, and returns the id of the copy the vm
// boots from, "" for a vm that is not seeded. it happens before the vm is
// created: the host agent resolves a seed id out of its own snapshot store. a
// copy that already landed is found again. the id is journaled, so a restart
// before the boot does not lose it.
func (fm *FleetManager) CreateArtifact(ctx context.Context, vmID string, req CreateRequest, st CreateState) (string, error) {
	v, err := fm.createVM(vmID)
	if err != nil {
		return "", err
	}
	run, err := fm.createRunFor(vmID, req, st)
	if err != nil {
		return "", err
	}
	fm.mu.RLock()
	spec := createSpec(v, run, st)
	fm.mu.RUnlock()
	if spec.SeedSnapshotID == "" {
		return "", nil
	}
	seed := fm.planSeedPlacement(ctx, spec)
	localID := spec.SeedSnapshotID
	if seed.needsMove(st.HostID) {
		if localID, err = fm.ensureArtifactOnHost(ctx, seed.record, st.HostID, false); err != nil {
			return "", createPermanentErr(fmt.Errorf("provision vm %s: %w", vmID, err))
		}
	}
	// the tracked spec is what the collector reads to see the copy in use.
	fm.mu.Lock()
	v.spec.SeedSnapshotID = localID
	v.spec.PinnedHostID = st.HostID
	fm.mu.Unlock()
	fm.touchArtifact(localID, seed.record.SnapshotID)
	return localID, nil
}

// CreatePublishIngress publishes the vm through fuse-proxy before it boots,
// so the guest's tunnel config goes up with the rest of its files. nil when
// there is no proxy or the vm cannot run the sidecar. a second publish for
// the same owner replaces the first, so a retry leaves the latest token.
func (fm *FleetManager) CreatePublishIngress(ctx context.Context, vmID string, req CreateRequest, st CreateState) (*CreateIngress, error) {
	v, err := fm.createVM(vmID)
	if err != nil {
		return nil, err
	}
	run, err := fm.createRunFor(vmID, req, st)
	if err != nil {
		return nil, err
	}
	fm.mu.RLock()
	spec := createSpec(v, run, st)
	fm.mu.RUnlock()
	if !fm.ingressSupported(st.HostID, spec) {
		return nil, nil
	}
	plan, err := fm.publishIngress(ctx, vmID, run.in.Expose, "")
	if err != nil || plan == nil {
		return nil, err
	}
	sealed, err := secrets.EncryptBytes(plan.tunnelConfig, fm.tokenEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("seal tunnel config: %w", err)
	}
	run.agentSpec = nil
	return &CreateIngress{TunnelConfig: sealed, Grant: plan.grant}, nil
}

// CreateRestoreOrCreate brings the vm up on its host: a restore of its own
// latest checkpoint when the host already has one by this name, else a
// create. the create is keyed on the step, and a vm an earlier attempt
// created is kept rather than created again. it reports a restore.
func (fm *FleetManager) CreateRestoreOrCreate(ctx context.Context, vmID string, req CreateRequest, st CreateState) (bool, error) {
	b, _, run, err := fm.createBoot(ctx, vmID, req, st, false)
	if err != nil {
		return false, err
	}
	env, fromCache, err := restoreOrCreate(fm.epochCtx(ctx, vmID), b.p, b.in.spec)
	if err != nil {
		return false, provisionErr(vmID, err, run.sensitive())
	}
	setTokenIfSupported(env, run.creds)
	run.env = env
	return fromCache, nil
}

// CreateProvisionEgress brings up the egress the spec asks for, releasing
// first what an earlier attempt or the restored vm's last life may have left
// behind, and persists a proxy's backend so a rollback after a restart still
// knows what to release. an egress backend that refuses fails the create.
func (fm *FleetManager) CreateProvisionEgress(ctx context.Context, vmID string, req CreateRequest, st CreateState) error {
	b, v, run, err := fm.createBoot(ctx, vmID, req, st, true)
	if err != nil {
		return err
	}
	if err := b.provisionEgress(ctx, true); err != nil {
		redacted := secrets.RedactSecretValues(err.Error(), run.sensitive())
		var egressErr *egress.ProvisionError
		if errors.As(err, &egressErr) {
			fm.appendEvent(ctx, "vm", vmID, "vm.egress_failed", map[string]any{
				"task_id":  req.TaskID,
				"provider": egressErr.Provider,
				"error":    redacted,
			})
			if em, ok := fm.metrics.(EgressMetrics); ok {
				em.EgressProvisionFailed(egressErr.Provider)
			}
		}
		return provisionErr(vmID, createPermanentErr(err), run.sensitive())
	}
	fm.mu.Lock()
	v.egress = b.egress
	fm.mu.Unlock()
	if b.egress.Mode != egress.ModeProxy {
		return nil
	}
	if err := fm.persistVM(ctx, v); err != nil {
		return fmt.Errorf("persist vm %s egress: %w", vmID, err)
	}
	return nil
}

// CreateUpload writes the agent profile's files and the egress files into
// the guest. each file is keyed apart on the step, so the concurrent uploads
// of a retried step key the same way whatever order they go out in.
func (fm *FleetManager) CreateUpload(ctx context.Context, vmID string, req CreateRequest, st CreateState) error {
	b, _, run, err := fm.createBoot(ctx, vmID, req, st, true)
	if err != nil {
		return err
	}
	return provisionErr(vmID, b.upload(ctx), run.sensitive())
}

// CreateStartupScript runs the caller's startup script. a script that exits
// non-zero or runs past its bound fails the create; only a failure to reach
// the guest is retried, and the exec is keyed, so a retry after the script
// finished gets its first result back instead of running it twice.
func (fm *FleetManager) CreateStartupScript(ctx context.Context, vmID string, req CreateRequest, st CreateState) error {
	b, _, run, err := fm.createBoot(ctx, vmID, req, st, true)
	if err != nil {
		return err
	}
	if b.in.opts.StartupScript == "" {
		return nil
	}
	err = b.startup(ctx)
	if err != nil && !unreachable(err) {
		err = createPermanentErr(err)
	}
	return provisionErr(vmID, err, run.sensitive())
}

// unreachable reports whether err is a failure to reach the host agent, or
// an agent that failed without an answer, rather than an answer.
func unreachable(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Code >= 500 || statusErr.Code == http.StatusRequestTimeout || statusErr.Code == http.StatusTooManyRequests
	}
	return false
}

// CreateStartAgent launches the guest agent and returns the endpoints it
// published.
func (fm *FleetManager) CreateStartAgent(ctx context.Context, vmID string, req CreateRequest, st CreateState) ([]Endpoint, error) {
	b, _, run, err := fm.createBoot(ctx, vmID, req, st, true)
	if err != nil {
		return nil, err
	}
	endpoints, err := b.startAgent(ctx)
	return endpoints, provisionErr(vmID, err, run.sensitive())
}

// CreateRunning records the vm running and its task running. it is the one
// point every successful create, durable or not, reaches once the vm is
// fully up; anything scheduled off a running vm (a ttl) starts here.
func (fm *FleetManager) CreateRunning(ctx context.Context, vmID string, req CreateRequest, st CreateState) error {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	done := ok && v.state == VMStateRunning && v.taskID == req.TaskID
	fm.mu.RUnlock()
	if done {
		// an earlier run of this step got as far as the writes.
		fm.dropCreateRun(vmID)
		return nil
	}
	b, v, run, err := fm.createBoot(ctx, vmID, req, st, true)
	if err != nil {
		return err
	}
	secretsEncrypted, err := fm.sealGuestSecrets(run.in.Secrets, b.in.spec.Egress)
	if err != nil {
		return err
	}
	status := b.egress
	if status.Mode == "" {
		status = egress.Status{Mode: egress.ModeDirect}
	}

	fm.mu.Lock()
	v.spec = b.in.spec
	v.env = b.env
	v.url = b.env.URL()
	v.state = VMStateRunning
	v.secretsEncrypted = secretsEncrypted
	v.drainCommand = b.in.agentSpec.DrainCommand
	v.endpoints = st.Endpoints
	v.egress = status
	v.healthcheck = run.in.Healthcheck
	v.updatedAt = time.Now()
	if st.Ingress != nil {
		v.applyIngress(st.Ingress.Grant)
	}
	createdAt := v.createdAt
	fm.mu.Unlock()
	if err := fm.persistVMByID(ctx, vmID); err != nil {
		// put the state back, so a retry still finds the vm being created.
		fm.mu.Lock()
		v.state = VMStateProvisioning
		fm.mu.Unlock()
		return fmt.Errorf("persist vm %s running state: %w", vmID, err)
	}
	if err := fm.store.UpsertTask(ctx, TaskRecord{
		TaskID:     req.TaskID,
		VMID:       vmID,
		RunStatus:  TaskRunRunning,
		AssignedAt: createdAt,
		UpdatedAt:  time.Now(),
	}); err != nil {
		fm.mu.Lock()
		v.state = VMStateProvisioning
		fm.mu.Unlock()
		return fmt.Errorf("persist task %s running state: %w", req.TaskID, err)
	}
	fm.dropCreateRun(vmID)
	fm.publishStateChange(vmID, "")
	fm.logger.InfoContext(ctx, "vm running", "vm", vmID, "task", req.TaskID, "from_cache", st.FromCache, "host", st.HostID)
	fm.appendEvent(ctx, "vm", vmID, "vm.running", map[string]any{"task_id": req.TaskID})
	fm.appendEvent(ctx, "task", req.TaskID, "task.running", map[string]any{"vm_id": vmID})
	fm.armExpiry(vmID)
	if status.Mode == egress.ModeProxy {
		// provider and protocol only: the endpoint is on the environment for
		// anyone who needs it, and never in an event or a log line.
		fm.appendEvent(ctx, "vm", vmID, "vm.egress_provisioned", map[string]any{
			"task_id":  req.TaskID,
			"provider": status.Provider,
			"protocol": string(status.Protocol),
		})
	}
	if len(run.in.Secrets) > 0 {
		fm.appendEvent(ctx, "vm", vmID, "secrets.deployed", map[string]any{
			"task_id":     req.TaskID,
			"secret_keys": secrets.SecretKeyNames(run.in.Secrets),
			"count":       len(run.in.Secrets),
		})
	}
	return nil
}

// sealGuestSecrets is the vm row's copy of the guest's secrets, which
// reuploadSecrets writes back after a restart: the guest's view, without the
// egress credential. nil without secrets.
func (fm *FleetManager) sealGuestSecrets(secretMap map[string]string, spec egress.Spec) ([]byte, error) {
	guestSecrets := GuestSecrets(secretMap, spec)
	if len(guestSecrets) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(guestSecrets)
	if err != nil {
		return nil, fmt.Errorf("encode secrets for persistence: %w", err)
	}
	sealed, err := secrets.EncryptBytes(raw, fm.tokenEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt secrets for persistence: %w", err)
	}
	return sealed, nil
}

// the compensation of a failed create, in order. each is best effort or
// retried until it lands; the handler then sends the vm's destroy through
// Environment/{vm_id}.Destroy, which restate runs once the create has ended.

// CreateReleaseEgress releases the proxy egress the create brought up. best
// effort, like every teardown's egress release: the destroy that follows
// releases whatever this missed.
func (fm *FleetManager) CreateReleaseEgress(ctx context.Context, vmID string) error {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok || v.state != VMStateProvisioning {
		fm.mu.Unlock()
		return nil
	}
	// the egress addresses come from the environment, which a create in
	// flight keeps on its run rather than on the vm.
	if run := fm.createRuns[vmID]; run != nil && v.env == nil {
		v.env = run.env
	}
	fm.mu.Unlock()
	fm.releaseEgress(ctx, v)
	return nil
}

// CreateUnpublishIngress removes the routes the create published.
func (fm *FleetManager) CreateUnpublishIngress(ctx context.Context, vmID string) error {
	fm.unpublishIngress(ctx, vmID)
	return nil
}

// CreateAbandon fails the create: the task is marked failed with cause, then
// the vm's row and reservation go and subscribers learn it failed. the task
// write goes first, so a replay after the row is gone still fails the task.
func (fm *FleetManager) CreateAbandon(ctx context.Context, vmID string, req CreateRequest, cause string) error {
	// the check and the removal from the fleet are one critical section, so
	// a step that makes the vm running meanwhile (a migrate's persist on a
	// target the stale pass is abandoning) either wins, and the vm is left
	// alone, or finds the vm gone and rolls back. a retry after a failed
	// write below finds the vm already gone and only redoes the writes.
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	ours := ok && v.state == VMStateProvisioning && v.taskID == req.TaskID
	createdAt := time.Now()
	if ours {
		createdAt = v.createdAt
		if v.hostID != "" {
			fm.deallocateOnHost(v.hostID, v.spec)
		}
		v.err = cause
		delete(fm.vms, vmID)
	}
	fm.mu.Unlock()
	if ok && !ours {
		// the vm id belongs to something else now; leave it alone.
		fm.dropCreateRun(vmID)
		return nil
	}
	if err := fm.store.UpsertTask(ctx, TaskRecord{
		TaskID:     req.TaskID,
		VMID:       vmID,
		RunStatus:  TaskRunFailed,
		LastError:  cause,
		AssignedAt: createdAt,
		UpdatedAt:  time.Now(),
	}); err != nil {
		return fmt.Errorf("persist task %s failed state: %w", req.TaskID, err)
	}
	if err := fm.store.DeleteVM(ctx, vmID); err != nil {
		return fmt.Errorf("delete vm %s row: %w", vmID, err)
	}
	fm.dropCreateRun(vmID)
	fm.logger.ErrorContext(ctx, "provision failed", "vm", vmID, "task", req.TaskID, "err", cause)
	fm.appendEvent(ctx, "vm", vmID, "vm.provision_failed", map[string]any{"task_id": req.TaskID, "error": cause})
	fm.appendEvent(ctx, "task", req.TaskID, "task.failed", map[string]any{"vm_id": vmID, "error": cause})
	fm.publishTerminalEvent(vmID, VMStateFailed, cause)
	return nil
}

// reconcileStaleCreates fails provisioning vms no create will finish: on
// restate, a vm recovered provisioning whose create ran in process under an
// older release has no invocation to resume it, and would otherwise hold its
// task and its host's capacity forever, since the rest of reconcile skips
// provisioning vms. only vms recovered provisioning at startup are asked
// about, never a create, fork or migrate this process is running. a vm
// restate cannot answer for is asked again next cycle.
//
// a migrate's target is not a create restate can be asked about: the migrate
// runs on its source's object. one still provisioning staleMigrateAfter after
// the migrate began has lost its migrate (an invocation killed, restate data
// lost); were the migrate still running, its next step would find the target
// gone and roll back with the source resumed.
func (fm *FleetManager) reconcileStaleCreates(ctx context.Context) {
	d := fm.durableCreator()
	if d == nil {
		return
	}
	type stale struct {
		id, taskID, hostID string
		migrate            bool
	}
	var candidates []stale
	fm.mu.Lock()
	for id := range fm.recoveredCreates {
		v, ok := fm.vms[id]
		if !ok || v.state != VMStateProvisioning {
			delete(fm.recoveredCreates, id)
			continue
		}
		if _, running := fm.createRuns[id]; running {
			// its create resumed in this process.
			delete(fm.recoveredCreates, id)
			continue
		}
		if fm.isMigratePlaceholder(v) && time.Since(v.createdAt) < staleMigrateAfter {
			continue
		}
		candidates = append(candidates, stale{id, v.taskID, v.hostID, fm.isMigratePlaceholder(v)})
	}
	fm.mu.Unlock()
	for _, c := range candidates {
		if c.migrate {
			fm.abandonStaleCreate(ctx, c.id, c.taskID, c.hostID, "migrate target left provisioning with no migrate to finish it")
			continue
		}
		askCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		inFlight, err := d.CreateInFlight(askCtx, c.id)
		cancel()
		if err != nil {
			fm.logger.Warn("ask restate about a recovered provisioning vm failed, retrying next cycle", "vm", c.id, "err", err)
			continue
		}
		if inFlight {
			fm.mu.Lock()
			delete(fm.recoveredCreates, c.id)
			fm.mu.Unlock()
			continue
		}
		fm.abandonStaleCreate(ctx, c.id, c.taskID, c.hostID, "provisioning interrupted with no create to finish it")
	}
}

// staleMigrateAfter is how long after its migrate began a recovered migrate
// target may stay provisioning: far past the longest a migrate's steps take
// (the lazy memory watch is LazyMemoryWatchTimeout).
const staleMigrateAfter = 24 * time.Hour

// abandonStaleCreate fails a recovered provisioning vm nothing will finish
// and destroys what it may have on its host. a vm never placed on a fleet
// with hosts has nothing to destroy; only a hostless fleet's default
// provider can hold it.
func (fm *FleetManager) abandonStaleCreate(ctx context.Context, vmID, taskID, hostID, cause string) {
	// read before the abandon drops the vm.
	fm.mu.RLock()
	var epoch int64
	if v, ok := fm.vms[vmID]; ok {
		epoch = v.epoch()
	}
	fm.mu.RUnlock()
	if err := fm.CreateAbandon(ctx, vmID, CreateRequest{TaskID: taskID}, cause); err != nil {
		fm.logger.Warn("abandon stale create failed, retrying next cycle", "vm", vmID, "err", err)
		return
	}
	fm.mu.Lock()
	delete(fm.recoveredCreates, vmID)
	hostless := len(fm.hosts) == 0
	fm.mu.Unlock()
	if hostID != "" || hostless {
		fm.cleanupFailedCreate(vmID, hostID, epoch)
	}
}
