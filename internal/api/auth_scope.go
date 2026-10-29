package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/folsomintel/fuse/internal/hitl"
)

// forbidden reasons, sent as details.reason on a 403 forbidden so an sdk can
// tell them apart without parsing the message.
const (
	reasonMissingScope    = "missing_scope"
	reasonEnvScope        = "env_scope"
	reasonSelfApproval    = "self_approval"
	reasonActorRequired   = "actor_required"
	reasonActorMismatch   = "actor_mismatch"
	reasonActorNotAllowed = "actor_not_allowed"
	reasonMintEscalation  = "mint_escalation"
)

func writeForbidden(w http.ResponseWriter, reason, msg string) {
	writeError(w, http.StatusForbidden, CodeForbidden, msg, map[string]string{"reason": reason})
}

// principal returns the request's principal. insecure mode, where
// BearerAuth sets none, is the master operator.
func principal(r *http.Request) Principal {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		return Principal{Master: true}
	}
	return p
}

// accessGuard runs after BearerAuth. it resolves who the request is
// recorded as from the key and the Fuse-Actor headers, records an invite's
// first use, and keeps a scoped key on the hitl routes: anything else is a
// 404 for an environment the key does not cover and a 403 otherwise.
func (h *Handler) accessGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		named, err := hitl.ParseActorHeaders(r.Header.Get(hitl.HeaderActor), r.Header.Get(hitl.HeaderActorName))
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, hitl.HeaderActor+": "+err.Error(), nil)
			return
		}
		cred := hitl.Credential{
			Master: p.Master,
			KeyID:  p.KeyID,
			Label:  p.Label,
			Pinned: p.PinnedActor,
			ActAs:  p.Scopes != nil && hitl.Grants(p.Scopes, hitl.ScopeActAs),
		}
		actor, err := hitl.ResolveActor(cred, named)
		switch {
		case errors.Is(err, hitl.ErrActorMismatch):
			writeForbidden(w, reasonActorMismatch, "this key is pinned to actor \""+p.PinnedActor.ID+"\"; a different "+hitl.HeaderActor+" is not allowed")
			return
		case errors.Is(err, hitl.ErrActorNotAllowed):
			writeForbidden(w, reasonActorNotAllowed, "this key may not name an actor ("+hitl.HeaderActor+" needs the hitl:act-as scope)")
			return
		case err != nil:
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
			return
		}
		p.Actor = actor

		if p.FirstUse && p.Scopes != nil && h.Fleet != nil {
			h.Fleet.RecordHitlAudit(r.Context(), hitl.AuditEntry{
				Event: hitl.AuditInviteUsed, EnvID: p.EnvID, Actor: actor, RemoteAddr: r.RemoteAddr,
			})
		}

		if p.Scopes != nil && !hitlRoute(r) {
			if env := envFromPath(r.URL.Path); env != "" && !h.covers(p, env) {
				writeError(w, http.StatusNotFound, CodeNotFound, "environment not found", nil)
				return
			}
			writeForbidden(w, reasonEnvScope, "this key is limited to the hitl routes")
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// hitlRoute reports whether a scoped key may reach the route at all. what
// it may do there is checked by requireScope in the handler.
func hitlRoute(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case path == "/v1/interventions", strings.HasPrefix(path, "/v1/interventions/"):
		return true
	case strings.HasPrefix(path, "/v1/hitl/"):
		return true
	case path == "/v1/api-keys" && r.Method == http.MethodPost:
		// minting, within the key's own scopes. see createAPIKey.
		return true
	}
	return false
}

// envFromPath returns the environment id of an /v1/environments/{id} route.
func envFromPath(path string) string {
	rest, ok := strings.CutPrefix(path, "/v1/environments/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// requireScope writes a 403 and returns false unless the caller holds want.
// the master token holds every scope.
func (h *Handler) requireScope(w http.ResponseWriter, r *http.Request, want hitl.Scope) bool {
	p := principal(r)
	if p.Master || hitl.Grants(p.Scopes, want) {
		return true
	}
	msg := "this request needs the " + string(want) + " scope"
	if p.Scopes == nil && want == hitl.ScopeDecide {
		msg += "; full-access keys do not hold it, so an agent's own key cannot approve its requests. mint a key with it (fuse hitl invite)"
	}
	writeError(w, http.StatusForbidden, CodeForbidden, msg, map[string]string{"reason": reasonMissingScope, "scope": string(want)})
	return false
}

// covers reports whether the caller may reach envID: any env for a key not
// scoped to one, else the key's env and its forks.
func (h *Handler) covers(p Principal, envID string) bool {
	if p.EnvID == "" {
		return true
	}
	return h.Fleet != nil && h.Fleet.EnvCovered(p.EnvID, envID)
}

// coveredEnvs is the env filter a list applies for the caller: nil for no
// limit, else the key's env and its live forks.
func (h *Handler) coveredEnvs(p Principal) []string {
	if p.EnvID == "" {
		return nil
	}
	return h.Fleet.EnvsCoveredBy(p.EnvID)
}

// writeHitlError maps the decider refusals to a 403 and everything else
// through writeFleetError.
func writeHitlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hitl.ErrSelfApproval):
		writeForbidden(w, reasonSelfApproval, err.Error())
	case errors.Is(err, hitl.ErrActorRequired):
		writeForbidden(w, reasonActorRequired, err.Error()+" (use a key pinned to an actor, or send "+hitl.HeaderActor+" from a hitl:act-as key)")
	default:
		writeFleetError(w, err)
	}
}
