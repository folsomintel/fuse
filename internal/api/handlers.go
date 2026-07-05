package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/secrets"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// ProviderFactory constructs a Provider for a registered host given
// its URL, auth token, and virtualization backend. The REST handler
// calls this during POST /v1/hosts to avoid importing provider-specific
// packages. In production a firecracker backend returns a
// firecracker.Provider and a qemu backend returns a qemu.Provider;
// tests pass a stub.
type ProviderFactory func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider

// Handler is the HTTP dependency graph for the orchestrator REST API.
type Handler struct {
	// Fleet is the FleetManager instance the handlers proxy to.
	// In production this is *orchestrator.FleetManager. Tests may
	// pass any implementation compatible with the Go method set.
	Fleet *orchestrator.FleetManager

	// Resolver turns inline manifest payloads into raw bytes.
	// Defaults to InlineResolver if left nil.
	Resolver Resolver

	// NewProvider constructs a Provider for a host URL. Required for
	// host registration (POST /v1/hosts). Nil means host registration
	// is disabled and returns 501.
	NewProvider ProviderFactory

	// AuthToken is the static Bearer token required for all endpoints.
	// Empty means no auth (insecure/dev mode).
	AuthToken string

	// APIKeys is the store of revocable API keys. When non-nil it is
	// consulted by BearerAuth as a second accept path (after the master
	// token) and backs the /v1/api-keys management endpoints. Nil
	// disables API-key auth entirely.
	APIKeys APIKeyStore

	// AllowedCIDRs is a list of CIDR blocks from which requests are
	// accepted. Empty means open access.
	AllowedCIDRs []string

	// SecureCookies sets the Secure attribute on the session cookie
	// issued by POST /login. The server wires this to whether it is
	// serving TLS; it must be true in any cross-site browser deployment
	// (SameSite=None requires Secure) and should be true in production.
	SecureCookies bool

	// OnAuthFailure is called on every rejected auth attempt. The
	// server wires this to the FleetManager's event append for audit.
	// The requestID argument is the per-request correlation ID set
	// by [RequestIDMiddleware]; it is non-empty for any request that
	// passed through the standard [Handler.Router] chain.
	OnAuthFailure AuthFailureFunc

	// OnIPReject is called when a request is rejected by the CIDR
	// allowlist. Wired the same way as OnAuthFailure.
	OnIPReject IPRejectFunc

	// Metrics holds Prometheus counter/histogram vecs for HTTP RED
	// metrics. When non-nil, a recording middleware is installed on
	// the router. The three fields correspond to the outputs of
	// orchestrator.NewPrometheusMetrics.
	MetricsRequestsTotal    *prometheus.CounterVec
	MetricsRequestDuration  *prometheus.HistogramVec
	MetricsRequestsInFlight prometheus.Gauge
}

// Router returns an http.Handler serving the orchestrator REST API.
// It is safe to mount at any path prefix; routes are registered with
// absolute paths so `chi.Mount` and `http.StripPrefix` both work.
//
// Middleware order (outermost first):
//  1. Request-ID — assigns/propagates X-Request-ID so all downstream
//     middleware and audit callbacks share a correlation ID.
//  2. Metrics — records RED metrics for every request, including
//     those rejected by CIDR or auth.
//  3. CIDR allowlist — rejects before auth so blocked IPs can't even
//     probe for valid tokens.
//  4. Bearer auth — rejects unauthenticated requests.
//  5. Route handlers.
func (h *Handler) Router() (http.Handler, error) {
	r := chi.NewRouter()

	// Request ID (outermost — every other layer can read RequestID(ctx)).
	r.Use(RequestIDMiddleware)

	// Metrics middleware (records all requests including those
	// rejected by auth or CIDR).
	if h.MetricsRequestsTotal != nil {
		r.Use(MetricsMiddleware(
			h.MetricsRequestsTotal,
			h.MetricsRequestDuration,
			h.MetricsRequestsInFlight,
		))
	}

	// CIDR allowlist (if configured).
	if len(h.AllowedCIDRs) > 0 {
		cidrMW, err := CIDRAllowlist(h.AllowedCIDRs, h.OnIPReject)
		if err != nil {
			return nil, fmt.Errorf("parse allowed CIDRs: %w", err)
		}
		r.Use(cidrMW)
	}

	// Auth endpoints live *outside* BearerAuth so an unauthenticated
	// browser can exchange the token for a session cookie. They still
	// sit behind the CIDR allowlist above. login itself constant-time
	// compares the posted token; logout just clears the cookie. chi
	// requires all r.Use middleware before any route, so these are
	// registered as their own group rather than after the BearerAuth
	// Use call below.
	r.Group(func(pub chi.Router) {
		pub.Post("/login", h.login)
		pub.Post("/logout", h.logout)
	})

	// Everything else requires the bearer token (header or session
	// cookie). The protected routes are registered inside a group that
	// applies BearerAuth, so the auth endpoints above stay open.
	r.Group(func(priv chi.Router) {
		var keyAuth APIKeyAuthenticator
		if h.APIKeys != nil {
			keyAuth = h.APIKeys
		}
		priv.Use(BearerAuth(h.AuthToken, keyAuth, h.OnAuthFailure))
		h.register(priv)
	})

	return r, nil
}

// /health and /ready live in health.go and are mounted on the outer
// mux in server/main.go (alongside /metrics) so they can be probed
// without a Bearer token. They intentionally do not pass through
// [RequestIDMiddleware] — probes don't need correlation IDs.

func (h *Handler) register(r chi.Router) {
	r.Get("/v1/environments", h.listEnvironments)
	r.Post("/v1/environments", h.createEnvironment)
	r.Get("/v1/environments/{vmId}", h.getEnvironment)
	r.Get("/v1/environments/{vmId}/events", h.streamEnvironmentEvents)
	r.Post("/v1/environments/{vmId}", h.environmentAction)
	r.Delete("/v1/environments/{vmId}", h.destroyEnvironment)

	r.Post("/v1/environments/{vmId}/snapshots", h.createSnapshot)
	r.Get("/v1/snapshots", h.listSnapshots)
	r.Get("/v1/snapshots/{snapshotId}", h.getSnapshot)
	r.Post("/v1/snapshots/{snapshotId}", h.snapshotAction)
	r.Delete("/v1/snapshots/{snapshotId}", h.deleteSnapshot)

	// API key management (master-token only; enforced per-handler).
	// Registered only when a key store is configured.
	if h.APIKeys != nil {
		r.Post("/v1/api-keys", h.createAPIKey)
		r.Get("/v1/api-keys", h.listAPIKeys)
		r.Delete("/v1/api-keys/{id}", h.revokeAPIKey)
	}

	// Host management (scheduler).
	r.Post("/v1/hosts", h.registerHost)
	r.Get("/v1/hosts", h.listHosts)
	r.Get("/v1/hosts/{hostId}", h.getHost)
	r.Post("/v1/hosts/{hostId}", h.hostAction)
	r.Delete("/v1/hosts/{hostId}", h.removeHost)
}

// resolver returns h.Resolver or the default inline resolver.
func (h *Handler) resolver() Resolver {
	if h.Resolver != nil {
		return h.Resolver
	}
	return InlineResolver{}
}

// ── Environment handlers ──────────────────────────────────────────

// createEnvironment provisions a new environment for a task.
//
//	@Summary		Create environment
//	@Description	Creates a VM, uploads the manifest, starts the guest agent, and assigns the task.
//	@Tags			environments
//	@Accept			json
//	@Produce		json
//	@Param			body	body		CreateEnvironmentRequest	true	"Environment spec"
//	@Success		201		{object}	Environment
//	@Failure		400		{object}	Error
//	@Failure		409		{object}	Error
//	@Failure		500		{object}	Error
//	@Failure		503		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/environments [post]
func (h *Handler) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var req CreateEnvironmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid JSON body: "+err.Error(), nil)
		return
	}
	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "task_id is required", nil)
		return
	}

	manifest, err := h.resolver().Resolve(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}

	if err := secrets.ValidateSecrets(manifest, req.Secrets); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}

	spec := toOrchestratorSpec(req.Spec)
	info, err := h.Fleet.ProvisionAndAssign(r.Context(), req.TaskID, spec, manifest, req.Secrets, orchestrator.BootOptions{
		StartupScript: req.StartupScript,
		GatewayURL:    req.GatewayURL,
		GatewayToken:  req.GatewayToken,
		Expose:        toOrchestratorExpose(req.Expose),
	})
	if err != nil {
		writeFleetErrorRedacted(w, err, req.Secrets)
		return
	}

	writeJSON(w, http.StatusCreated, toAPIEnvironment(*info))
}

// listEnvironments returns tracked environments with optional filters.
//
//	@Summary	List environments
//	@Tags		environments
//	@Produce	json
//	@Param		task_id	query		string	false	"Filter by task ID"
//	@Param		state	query		string	false	"Filter by state"	Enums(provisioning, running, destroying)
//	@Param		host_id	query		string	false	"Filter by host ID"
//	@Success	200		{object}	EnvironmentList
//	@Security	BearerAuth
//	@Router		/v1/environments [get]
func (h *Handler) listEnvironments(w http.ResponseWriter, r *http.Request) {
	filter := orchestrator.VMFilter{
		TaskID: r.URL.Query().Get("task_id"),
		State:  orchestrator.VMState(r.URL.Query().Get("state")),
		HostID: r.URL.Query().Get("host_id"),
	}

	fleet := h.Fleet.ListFleetFiltered(filter)
	out := EnvironmentList{Environments: make([]Environment, 0, len(fleet))}
	for _, v := range fleet {
		out.Environments = append(out.Environments, toAPIEnvironment(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// getEnvironment fetches a single environment by VM ID.
//
//	@Summary	Get environment
//	@Tags		environments
//	@Produce	json
//	@Param		vmId	path		string	true	"VM identifier"
//	@Success	200		{object}	Environment
//	@Failure	404		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/environments/{vmId} [get]
func (h *Handler) getEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	info, ok := h.Fleet.GetVM(vmID)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "vm "+vmID+" not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, toAPIEnvironment(info))
}

// destroyEnvironment tears down a VM.
//
//	@Summary		Destroy environment
//	@Description	Forcefully destroys the VM and marks its task failed.
//	@Tags			environments
//	@Param			vmId	path	string	true	"VM identifier"
//	@Success		204		"VM destroyed"
//	@Failure		404		{object}	Error
//	@Failure		500		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/environments/{vmId} [delete]
func (h *Handler) destroyEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	if err := h.Fleet.DestroyVM(r.Context(), vmID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Snapshot handlers ─────────────────────────────────────────────

// createSnapshot checkpoints a running VM.
//
//	@Summary	Create snapshot
//	@Tags		snapshots
//	@Accept		json
//	@Produce	json
//	@Param		vmId	path		string					true	"VM identifier"
//	@Param		body	body		CreateSnapshotRequest	false	"Snapshot options"
//	@Success	201		{object}	Snapshot
//	@Failure	404		{object}	Error
//	@Failure	409		{object}	Error
//	@Failure	500		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/environments/{vmId}/snapshots [post]
func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")

	var req CreateSnapshotRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid JSON body: "+err.Error(), nil)
			return
		}
	}

	var retentionUntil *time.Time
	if req.RetentionSeconds > 0 {
		t := time.Now().Add(time.Duration(req.RetentionSeconds) * time.Second)
		retentionUntil = &t
	}
	var exports []orchestrator.SnapshotExportRecord
	if req.ExportRef != "" {
		exports = append(exports, orchestrator.SnapshotExportRecord{
			Destination: req.ExportRef,
			Status:      orchestrator.SnapshotExportStatus(req.ExportStatus),
			RequestedAt: time.Now(),
			UpdatedAt:   time.Now(),
		})
		if exports[0].Status == "" {
			exports[0].Status = orchestrator.SnapshotExportPending
		}
	}

	record, err := h.Fleet.CreateSnapshot(r.Context(), vmID, orchestrator.SnapshotOptions{
		Comment:        req.Comment,
		Mode:           orchestrator.SnapshotMode(req.Mode),
		RetentionUntil: retentionUntil,
		Metadata:       req.Metadata,
		Exports:        exports,
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := toAPISnapshot(record)
	if req.Comment != "" && out.Comment == "" {
		out.Comment = req.Comment
	}
	if req.ExportRef != "" && out.ExportRef == "" {
		out.ExportRef = req.ExportRef
	}
	writeJSON(w, http.StatusCreated, out)
}

// listSnapshots returns snapshots with optional filters.
//
//	@Summary	List snapshots
//	@Tags		snapshots
//	@Produce	json
//	@Param		vm_id		query		string	false	"Filter by VM ID"
//	@Param		task_id		query		string	false	"Filter by task ID"
//	@Param		tenant_id	query		string	false	"Filter by tenant ID"
//	@Param		state		query		string	false	"Filter by state"	Enums(creating, ready, restoring, deleting, error)
//	@Success	200			{object}	SnapshotList
//	@Security	BearerAuth
//	@Router		/v1/snapshots [get]
func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	filter := orchestrator.SnapshotFilter{
		VMID:     r.URL.Query().Get("vm_id"),
		TaskID:   r.URL.Query().Get("task_id"),
		TenantID: r.URL.Query().Get("tenant_id"),
		State:    orchestrator.SnapshotState(r.URL.Query().Get("state")),
	}

	records, err := h.Fleet.ListSnapshotsFiltered(r.Context(), filter)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := SnapshotList{Snapshots: make([]Snapshot, 0, len(records))}
	for _, s := range records {
		out.Snapshots = append(out.Snapshots, toAPISnapshot(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// getSnapshot fetches a single snapshot by ID.
//
//	@Summary	Get snapshot
//	@Tags		snapshots
//	@Produce	json
//	@Param		snapshotId	path		string	true	"Snapshot identifier"
//	@Success	200			{object}	Snapshot
//	@Failure	404			{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/snapshots/{snapshotId} [get]
func (h *Handler) getSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")
	record, err := h.Fleet.GetSnapshotByID(r.Context(), snapshotID)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPISnapshot(record))
}

// deleteSnapshot removes a leaf snapshot.
//
//	@Summary	Delete snapshot
//	@Tags		snapshots
//	@Param		snapshotId	path	string	true	"Snapshot identifier"
//	@Success	204			"Snapshot deleted"
//	@Failure	404			{object}	Error
//	@Failure	409			{object}	Error
//	@Failure	500			{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/snapshots/{snapshotId} [delete]
func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")
	if err := h.Fleet.DeleteSnapshotByID(r.Context(), snapshotID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Snapshot actions ──────────────────────────────────────────────

// snapshotAction dispatches an action on a snapshot via ?action= query param.
//
//	@Summary		Snapshot action
//	@Description	Perform an action on a snapshot. Currently supports: restore.
//	@Tags			snapshots
//	@Param			snapshotId	path	string	true	"Snapshot identifier"
//	@Param			action		query	string	true	"Action to perform"	Enums(restore)
//	@Success		204			"Action succeeded"
//	@Failure		400			{object}	Error
//	@Failure		404			{object}	Error
//	@Failure		409			{object}	Error
//	@Failure		500			{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/snapshots/{snapshotId} [post]
func (h *Handler) snapshotAction(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("action") {
	case "restore":
		h.restoreSnapshot(w, r)
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"missing or unknown action query parameter", nil)
	}
}

func (h *Handler) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")
	if err := h.Fleet.RestoreSnapshotByID(r.Context(), snapshotID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Environment actions ───────────────────────────────────────────

// environmentAction dispatches an action on an environment via ?action= query param.
//
//	@Summary		Environment action
//	@Description	Perform an action on an environment. Supports: rotate-token, drain, fork.
//	@Tags			environments
//	@Param			vmId	path	string	true	"VM identifier"
//	@Param			action	query	string	true	"Action to perform"	Enums(rotate-token, drain, fork)
//	@Success		200		{object}	Environment	"Drain succeeded; updated VM state returned"
//	@Success		201		{object}	Environment	"Fork succeeded; new environment returned"
//	@Success		204		"Action succeeded"
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		409		{object}	Error
//	@Failure		500		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/environments/{vmId} [post]
func (h *Handler) environmentAction(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("action") {
	case "rotate-token":
		h.rotateToken(w, r)
	case "drain":
		h.drainEnvironment(w, r)
	case "fork":
		h.forkEnvironment(w, r)
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"missing or unknown action query parameter", nil)
	}
}

func (h *Handler) rotateToken(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	if err := h.Fleet.RotateToken(r.Context(), vmID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// drainEnvironment is the first phase of two-phase teardown. It runs the
// configured drain command inside the guest to gracefully quiesce in-guest
// workloads (an empty command leaves the VM Draining for the caller to
// DELETE), then leaves the VM in the Draining state so the caller can inspect
// outputs before issuing DELETE. See FleetManager.Drain for the full
// state-machine contract.
//
// Unlike most environment actions this returns 200 with the updated
// Environment body (rather than 204) so the caller can observe the
// state transition without a follow-up GET.
func (h *Handler) drainEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	if err := h.Fleet.Drain(r.Context(), vmID); err != nil {
		writeFleetError(w, err)
		return
	}
	info, ok := h.Fleet.GetVM(vmID)
	if !ok {
		// Drain succeeded but the VM disappeared between the call
		// and the read — extremely unlikely, but return 204 rather
		// than synthesising a stale body.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, toAPIEnvironment(info))
}

// forkEnvironment creates a new environment seeded from an existing one and
// returns it with 201. an optional body selects an existing snapshot to fork
// from (reuse_snapshot_id) and a comment; an empty body snapshots the source
// first. the new vm id is resolved via GetVM, mirroring drainEnvironment.
func (h *Handler) forkEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")

	var req ForkEnvironmentRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid JSON body: "+err.Error(), nil)
			return
		}
	}

	newID, err := h.Fleet.ForkEnvironment(r.Context(), vmID, orchestrator.ForkOptions{
		ReuseSnapshotID: req.ReuseSnapshotID,
		Comment:         req.Comment,
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	info, ok := h.Fleet.GetVM(newID)
	if !ok {
		// fork reported success but the new vm is not readable yet; return
		// 204 rather than synthesising a stale body.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIEnvironment(info))
}

// ── Host management handlers ──────────────────────────────────────

// hostAction dispatches an action on a host via ?action= query param.
//
//	@Summary		Host action
//	@Description	Perform an action on a host. Supports: cordon (mark unschedulable), uncordon (return to scheduling).
//	@Tags			hosts
//	@Param			hostId	path	string	true	"Host identifier"
//	@Param			action	query	string	true	"Action to perform"	Enums(cordon, uncordon)
//	@Success		204		"Action succeeded"
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		500		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/hosts/{hostId} [post]
func (h *Handler) hostAction(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("action") {
	case "cordon":
		h.cordonHost(w, r)
	case "uncordon":
		h.uncordonHost(w, r)
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"missing or unknown action query parameter", nil)
	}
}

// registerHost registers or updates a host in the scheduler.
//
//	@Summary	Register host
//	@Tags		hosts
//	@Accept		json
//	@Produce	json
//	@Param		body	body		RegisterHostRequest	true	"Host registration"
//	@Success	201		{object}	HostInfo
//	@Failure	400		{object}	Error
//	@Failure	500		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/hosts [post]
func (h *Handler) registerHost(w http.ResponseWriter, r *http.Request) {
	if h.NewProvider == nil {
		writeError(w, http.StatusNotImplemented, CodeInternal,
			"host registration is disabled (no provider factory configured)", nil)
		return
	}

	var req RegisterHostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "invalid JSON body: "+err.Error(), nil)
		return
	}
	if req.ID == "" || req.URL == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "id and url are required", nil)
		return
	}
	// TODO: Validate host capacity fields are positive (CPUs, RamMB, StorageGB,
	// VMCount). Currently accepts zero/negative values which would cause the
	// scheduler to make bad placement decisions.

	backend := orchestrator.HostBackend(req.Backend)
	if backend == "" {
		backend = orchestrator.BackendFirecracker
	}
	if req.Capacity.GPUs > 0 && backend != orchestrator.BackendQEMU {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"gpus > 0 requires backend \"qemu\"", nil)
		return
	}

	host := orchestrator.Host{
		ID:      req.ID,
		URL:     req.URL,
		Token:   req.Token,
		Region:  req.Region,
		Backend: backend,
		Capacity: orchestrator.HostCapacity{
			CPUs:      req.Capacity.CPUs,
			RamMB:     req.Capacity.RamMB,
			StorageGB: req.Capacity.StorageGB,
			VMCount:   req.Capacity.VMCount,
			GPUs:      req.Capacity.GPUs,
			GPUKind:   req.Capacity.GPUKind,
		},
	}

	provider := h.NewProvider(req.URL, req.Token, backend)
	if err := h.Fleet.RegisterHost(r.Context(), host, provider); err != nil {
		writeFleetError(w, err)
		return
	}

	info, ok := h.Fleet.GetHost(req.ID)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "host registered but not found", nil)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIHost(info))
}

// listHosts returns all registered hosts.
//
//	@Summary	List hosts
//	@Tags		hosts
//	@Produce	json
//	@Success	200	{object}	HostList
//	@Security	BearerAuth
//	@Router		/v1/hosts [get]
func (h *Handler) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts := h.Fleet.ListHosts()
	out := HostList{Hosts: make([]HostInfo, 0, len(hosts))}
	for _, host := range hosts {
		out.Hosts = append(out.Hosts, toAPIHost(host))
	}
	writeJSON(w, http.StatusOK, out)
}

// getHost fetches a single host by ID.
//
//	@Summary	Get host
//	@Tags		hosts
//	@Produce	json
//	@Param		hostId	path		string	true	"Host identifier"
//	@Success	200		{object}	HostInfo
//	@Failure	404		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/hosts/{hostId} [get]
func (h *Handler) getHost(w http.ResponseWriter, r *http.Request) {
	hostID := chi.URLParam(r, "hostId")
	host, ok := h.Fleet.GetHost(hostID)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "host "+hostID+" not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, toAPIHost(host))
}

func (h *Handler) cordonHost(w http.ResponseWriter, r *http.Request) {
	hostID := chi.URLParam(r, "hostId")
	if err := h.Fleet.CordonHost(r.Context(), hostID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) uncordonHost(w http.ResponseWriter, r *http.Request) {
	hostID := chi.URLParam(r, "hostId")
	if err := h.Fleet.UncordonHost(r.Context(), hostID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeHost removes a host from the scheduler.
//
//	@Summary	Remove host
//	@Tags		hosts
//	@Param		hostId	path	string	true	"Host identifier"
//	@Success	204		"Host removed"
//	@Failure	404		{object}	Error
//	@Failure	409		{object}	Error
//	@Failure	500		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/hosts/{hostId} [delete]
func (h *Handler) removeHost(w http.ResponseWriter, r *http.Request) {
	hostID := chi.URLParam(r, "hostId")
	if err := h.Fleet.RemoveHost(r.Context(), hostID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
