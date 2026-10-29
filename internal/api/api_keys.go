package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/apikeys"
	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/go-chi/chi/v5"
)

// APIKeyStore is the subset of the orchestrator's API key store that the
// REST handlers depend on. It is an interface so handlers can be tested
// with a stub and so the api package does not couple to *sql.DB. It also
// satisfies APIKeyAuthenticator (via Authenticate), so the same value
// wires into BearerAuth.
type APIKeyStore interface {
	Authenticate(ctx context.Context, rawToken string) (apikeys.APIKeyRecord, bool)
	Create(ctx context.Context, k apikeys.NewKey, now time.Time) (apikeys.APIKeyRecord, string, error)
	List(ctx context.Context) ([]apikeys.APIKeyRecord, error)
	Revoke(ctx context.Context, id string, now time.Time) error
}

// requireMaster writes a 403 and returns false unless the request
// authenticated with the master token. Key management is operator-only: a
// caller holding an API key must not be able to mint, list, or revoke
// keys. In insecure mode (no principal set) the request is treated as
// master, matching BearerAuth's open pass-through.
func (h *Handler) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	return h.requireMasterToken(w, r, "API key management requires the master token")
}

// requireMasterToken is requireMaster with a caller-supplied message, so
// operator-only endpoints other than key management can explain themselves.
func (h *Handler) requireMasterToken(w http.ResponseWriter, r *http.Request, msg string) bool {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.Master {
		return true // insecure mode, or genuine master
	}
	writeError(w, http.StatusForbidden, CodeUnauthorized, msg, nil)
	return false
}

func toAPIKey(rec apikeys.APIKeyRecord) APIKey {
	out := APIKey{
		ID:         rec.ID,
		Label:      rec.Label,
		CreatedAt:  rec.CreatedAt,
		LastUsedAt: rec.LastUsedAt,
		RevokedAt:  rec.RevokedAt,
		Scopes:     rec.Scopes,
		EnvID:      rec.EnvID,
		ExpiresAt:  rec.ExpiresAt,
		CreatedBy:  rec.CreatedBy,
	}
	if rec.ActorID != "" {
		out.Actor = &ActorLabel{ID: rec.ActorID, Name: rec.ActorName}
	}
	return out
}

// mintError is a refused mint: a status, a message and, for a 403, the
// forbidden reason.
type mintError struct {
	status int
	reason string
	msg    string
}

// planKey validates a mint request against the caller and returns the key
// to create. the master token may mint anything; a scoped key may mint only
// a narrower copy of itself (no escalation); a full-access key may not mint
// at all, as before scopes existed.
func (h *Handler) planKey(p Principal, req CreateAPIKeyRequest, now time.Time) (apikeys.NewKey, *mintError) {
	bad := func(format string, a ...any) *mintError {
		return &mintError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
	}
	escalate := func(msg string) *mintError {
		return &mintError{status: http.StatusForbidden, reason: reasonMintEscalation, msg: msg}
	}
	if !p.Master && p.Scopes == nil {
		return apikeys.NewKey{}, &mintError{status: http.StatusForbidden, reason: reasonMissingScope,
			msg: "API key management requires the master token"}
	}

	k := apikeys.NewKey{Label: req.Label, EnvID: req.EnvID, CreatedBy: "master"}
	if !p.Master {
		k.CreatedBy = p.KeyID
	}
	if req.TTLSeconds < 0 {
		return k, bad("ttl_seconds must not be negative")
	}
	var scopes []hitl.Scope
	if req.Scopes != nil {
		var err error
		if scopes, err = hitl.ParseScopes(req.Scopes); err != nil {
			return k, bad("scopes: %v", err)
		}
		if len(scopes) == 0 {
			return k, bad("scopes must not be empty; omit it for a full-access key")
		}
		k.Scopes = make([]string, 0, len(scopes))
		for _, s := range scopes {
			k.Scopes = append(k.Scopes, string(s))
		}
		if req.TTLSeconds == 0 {
			return k, bad("a key with scopes must expire: set ttl_seconds (at most %d)", int64(hitl.MaxScopedKeyTTL/time.Second))
		}
		if time.Duration(req.TTLSeconds)*time.Second > hitl.MaxScopedKeyTTL {
			return k, bad("ttl_seconds must be at most %d for a key with scopes", int64(hitl.MaxScopedKeyTTL/time.Second))
		}
	}
	if req.TTLSeconds > 0 {
		at := now.Add(time.Duration(req.TTLSeconds) * time.Second)
		k.ExpiresAt = &at
	}
	if req.EnvID != "" && scopes == nil {
		return k, bad("env_id needs scopes; a full-access key cannot be limited to one environment")
	}
	if req.Actor != nil {
		if err := hitl.ValidateActor(req.Actor.ID, req.Actor.Name); err != nil {
			return k, bad("actor: %v", err)
		}
		k.ActorID, k.ActorName = req.Actor.ID, req.Actor.Name
	}

	if !p.Master {
		// a scoped key mints only what it already has.
		if scopes == nil {
			return k, escalate("a scoped key cannot mint a full-access key")
		}
		for _, s := range scopes {
			if !hitl.Grants(p.Scopes, s) {
				return k, escalate("a key cannot mint a scope it does not hold: " + string(s))
			}
		}
		if p.EnvID != "" && (req.EnvID == "" || !h.covers(p, req.EnvID)) {
			return k, escalate("this key is scoped to environment " + p.EnvID + "; the new key must be scoped to it or one of its forks")
		}
		if p.ExpiresAt != nil && k.ExpiresAt.After(*p.ExpiresAt) {
			return k, escalate("the new key cannot outlive this one (expires " + p.ExpiresAt.UTC().Format(time.RFC3339) + ")")
		}
		switch {
		case p.PinnedActor != nil && k.ActorID == "":
			k.ActorID, k.ActorName = p.PinnedActor.ID, p.PinnedActor.Name
		case p.PinnedActor != nil && k.ActorID != p.PinnedActor.ID:
			return k, escalate("this key is pinned to actor " + p.PinnedActor.ID + " and cannot mint a key for anyone else")
		case p.PinnedActor == nil && k.ActorID != "" && !hitl.Grants(p.Scopes, hitl.ScopeActAs):
			return k, escalate("pinning an actor needs the hitl:act-as scope")
		}
	}
	if req.EnvID != "" && h.Fleet != nil {
		if _, ok := h.Fleet.GetVM(req.EnvID); !ok {
			return k, &mintError{status: http.StatusNotFound, msg: "environment " + req.EnvID + " not found"}
		}
	}
	return k, nil
}

// createAPIKey mints a new API key and returns the raw secret exactly once.
//
//	@Summary		Create API key
//	@Description	Mints a new revocable API key. The raw key is returned once and cannot be recovered. Without scopes the key is full-access and only the master token may mint it; with scopes it is limited to the hitl routes, must expire within 24h, and may be scoped to an environment and pinned to an actor. A scoped key may mint a narrower copy of itself.
//	@Tags			api-keys
//	@Accept			json
//	@Produce		json
//	@Param			body	body		CreateAPIKeyRequest	false	"Optional label"
//	@Success		201		{object}	CreateAPIKeyResponse
//	@Failure		401		{object}	Error
//	@Failure		403		{object}	Error
//	@Router			/v1/api-keys [post]
func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var req CreateAPIKeyRequest
	// Body is optional; only reject genuinely malformed or oversized JSON.
	if !decodeOptionalJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}

	p := principal(r)
	now := time.Now().UTC()
	k, merr := h.planKey(p, req, now)
	if merr != nil {
		switch merr.status {
		case http.StatusForbidden:
			writeForbidden(w, merr.reason, merr.msg)
		case http.StatusNotFound:
			writeError(w, merr.status, CodeNotFound, merr.msg, nil)
		default:
			writeError(w, merr.status, CodeInvalidArgument, merr.msg, nil)
		}
		return
	}

	rec, rawKey, err := h.APIKeys.Create(r.Context(), k, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"could not create api key", nil)
		return
	}
	if rec.Scopes != nil && h.Fleet != nil {
		h.Fleet.RecordHitlAudit(r.Context(), hitl.AuditEntry{
			Event: hitl.AuditInviteMinted, EnvID: rec.EnvID, Actor: p.Actor, RemoteAddr: r.RemoteAddr,
		})
	}

	writeJSON(w, http.StatusCreated, CreateAPIKeyResponse{
		APIKey: toAPIKey(rec),
		Key:    rawKey,
	})
}

// listAPIKeys returns metadata for all keys. It never returns key secrets.
//
//	@Summary		List API keys
//	@Description	Lists API key metadata (never the keys themselves).
//	@Tags			api-keys
//	@Produce		json
//	@Success		200	{object}	APIKeyList
//	@Failure		401	{object}	Error
//	@Failure		403	{object}	Error
//	@Router			/v1/api-keys [get]
func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !h.requireMaster(w, r) {
		return
	}

	recs, err := h.APIKeys.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"could not list api keys", nil)
		return
	}

	keys := make([]APIKey, 0, len(recs))
	for _, rec := range recs {
		keys = append(keys, toAPIKey(rec))
	}
	writeJSON(w, http.StatusOK, APIKeyList{APIKeys: keys})
}

// revokeAPIKey revokes the key with the given id.
//
//	@Summary		Revoke API key
//	@Description	Revokes an API key by id. Idempotent.
//	@Tags			api-keys
//	@Param			id	path	string	true	"API key id"
//	@Success		204
//	@Failure		401	{object}	Error
//	@Failure		403	{object}	Error
//	@Failure		404	{object}	Error
//	@Router			/v1/api-keys/{id} [delete]
func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireMaster(w, r) {
		return
	}

	id := chi.URLParam(r, "id")
	err := h.APIKeys.Revoke(r.Context(), id, time.Now().UTC())
	switch {
	case errors.Is(err, apikeys.ErrAPIKeyNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "api key not found", nil)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"could not revoke api key", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
