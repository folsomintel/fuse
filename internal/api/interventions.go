package api

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/go-chi/chi/v5"
)

// maxInterventionWait caps one long-poll. it sits under the server's
// default 60s write timeout; a caller that wants longer polls again.
const maxInterventionWait = 50 * time.Second

// requestHuman raises a request for a human on an environment
// (POST /v1/environments/{vmId}?action=request). it returns 201 at once with
// the pending intervention; the caller waits on its id with
// GET /v1/interventions/{id}?wait=.
func (h *Handler) requestHuman(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	var req RequestHumanRequest
	if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}
	kind := hitl.Kind(req.Kind)
	if kind == "" {
		kind = hitl.KindAsk
	}
	if kind != hitl.KindAsk {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "kind must be \"ask\", got \""+req.Kind+"\"", nil)
		return
	}
	in, err := h.Fleet.RequestHuman(r.Context(), vmID, orchestrator.HumanRequest{
		Kind:       kind,
		Ask:        hitl.AskPayload{Question: req.Question, Choices: req.Choices},
		Timeout:    time.Duration(req.TimeoutSeconds) * time.Second,
		By:         principal(r).Actor,
		RemoteAddr: r.RemoteAddr,
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIIntervention(in))
}

// listInterventions lists interventions, oldest first.
//
//	@Summary	List interventions
//	@Tags		interventions
//	@Produce	json
//	@Param		env		query		string	false	"Filter by environment id"
//	@Param		state	query		string	false	"Filter by state"	Enums(pending, approved, rejected, expired, cancelled)
//	@Param		actor	query		string	false	"Filter by the actor id that raised or decided it"
//	@Param		limit	query		int		false	"Max results per page (default 50, max 200)"
//	@Param		cursor	query		string	false	"Opaque pagination cursor from a previous response's next_cursor"
//	@Success	200		{object}	InterventionList
//	@Failure	400		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/interventions [get]
func (h *Handler) listInterventions(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeRead) {
		return
	}
	p := principal(r)
	env := r.URL.Query().Get("env")
	if env != "" && !h.covers(p, env) {
		writeError(w, http.StatusNotFound, CodeNotFound, "environment not found", nil)
		return
	}
	limit, cursor := parsePagination(r)
	state := hitl.State(r.URL.Query().Get("state"))
	if state != "" && !state.Valid() {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "unknown state \""+string(state)+"\"", nil)
		return
	}
	list, next, err := h.Fleet.ListInterventions(r.Context(), orchestrator.InterventionListFilter{
		EnvID:   env,
		EnvIDs:  h.coveredEnvs(p),
		ActorID: r.URL.Query().Get("actor"),
		State:   state,
		Limit:   limit,
		Cursor:  cursor,
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := InterventionList{Interventions: make([]Intervention, 0, len(list))}
	for _, in := range list {
		out.Interventions = append(out.Interventions, toAPIIntervention(in))
	}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// getIntervention reads one intervention. with ?wait= it long-polls until
// the intervention resolves or the wait passes, whichever is first, and
// always answers 200 with the state at that moment.
//
//	@Summary	Get intervention
//	@Tags		interventions
//	@Produce	json
//	@Param		interventionId	path		string	true	"Intervention id"
//	@Param		wait			query		string	false	"Long-poll up to this long for a terminal state (seconds or a go duration, max 50s)"
//	@Success	200				{object}	Intervention
//	@Failure	400				{object}	Error
//	@Failure	404				{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/interventions/{interventionId} [get]
func (h *Handler) getIntervention(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeRead) {
		return
	}
	id := chi.URLParam(r, "interventionId")
	wait, ok := parseWait(r.URL.Query().Get("wait"))
	if !ok {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "wait must be seconds or a duration like 30s", nil)
		return
	}
	if !h.interventionVisible(w, r, id) {
		return
	}
	if wait > 0 {
		// keep the server's write timeout from cutting the poll short.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + 10*time.Second))
	}
	in, err := h.Fleet.WaitIntervention(r.Context(), id, wait)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIIntervention(in))
}

// interventionVisible writes a 404 and returns false when the intervention
// does not exist or belongs to an environment the caller's key does not
// cover, so a scoped key cannot tell the two apart.
func (h *Handler) interventionVisible(w http.ResponseWriter, r *http.Request, id string) bool {
	p := principal(r)
	if p.EnvID == "" {
		return true
	}
	in, err := h.Fleet.GetIntervention(r.Context(), id)
	if err != nil {
		writeFleetError(w, err)
		return false
	}
	if !h.covers(p, in.EnvID) {
		writeError(w, http.StatusNotFound, CodeNotFound, "intervention not found", nil)
		return false
	}
	return true
}

// interventionAction decides an intervention: ?action=approve or reject.
// deciding needs the hitl:decide scope (or the master token), which
// full-access keys do not hold, and is never allowed to the key or actor
// that raised the intervention.
//
//	@Summary		Decide intervention
//	@Description	Approve (with an answer, for an ask) or reject (with a reason) a pending intervention. Requires hitl:decide or the master token; the key and actor that raised it can never decide it.
//	@Tags			interventions
//	@Accept			json
//	@Produce		json
//	@Param			interventionId	path		string						true	"Intervention id"
//	@Param			action			query		string						true	"Decision"	Enums(approve, reject)
//	@Param			body			body		DecideInterventionRequest	false	"Answer and reason"
//	@Success		200				{object}	Intervention
//	@Failure		400				{object}	Error
//	@Failure		403				{object}	Error
//	@Failure		404				{object}	Error
//	@Failure		409				{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/interventions/{interventionId} [post]
func (h *Handler) interventionAction(w http.ResponseWriter, r *http.Request) {
	var outcome hitl.Outcome
	switch r.URL.Query().Get("action") {
	case "approve":
		outcome = hitl.OutcomeApprove
	case "reject":
		outcome = hitl.OutcomeReject
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"missing or unknown action query parameter", nil)
		return
	}
	if !h.requireScope(w, r, hitl.ScopeDecide) {
		return
	}
	id := chi.URLParam(r, "interventionId")
	if !h.interventionVisible(w, r, id) {
		return
	}
	var req DecideInterventionRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
			return
		}
	}
	in, err := h.Fleet.DecideIntervention(r.Context(), id, orchestrator.DecideRequest{
		Outcome:    outcome,
		Answer:     req.Answer,
		Reason:     req.Reason,
		By:         principal(r).Actor,
		RemoteAddr: r.RemoteAddr,
	})
	if err != nil {
		writeHitlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIIntervention(in))
}

// parseWait reads ?wait= as whole seconds or a go duration, capped at
// maxInterventionWait. empty means no wait.
func parseWait(raw string) (time.Duration, bool) {
	if raw == "" {
		return 0, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, false
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 {
		return 0, false
	}
	return min(d, maxInterventionWait), true
}

// listHitlAudit reads the hitl audit log, oldest first.
//
//	@Summary		List hitl audit log
//	@Description	Reads the hash-chained hitl audit log: every raise, decision, expiry, cancel and invite, with actor, key and remote address, never content. A key scoped to an environment sees only that environment's rows.
//	@Tags			interventions
//	@Produce		json
//	@Param			env		query		string	false	"Filter by environment id"
//	@Param			actor	query		string	false	"Filter by actor id"
//	@Param			since	query		string	false	"Only rows at or after this time (RFC 3339) or this long ago (a go duration like 24h)"
//	@Param			limit	query		int		false	"Max results per page (default 50, max 200)"
//	@Param			cursor	query		string	false	"Opaque pagination cursor from a previous response's next_cursor"
//	@Success		200		{object}	AuditList
//	@Failure		400		{object}	Error
//	@Failure		403		{object}	Error
//	@Failure		404		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/hitl/audit [get]
func (h *Handler) listHitlAudit(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeRead) {
		return
	}
	p := principal(r)
	q := r.URL.Query()
	envIDs := h.coveredEnvs(p)
	if env := q.Get("env"); env != "" {
		if !h.covers(p, env) {
			writeError(w, http.StatusNotFound, CodeNotFound, "environment not found", nil)
			return
		}
		envIDs = []string{env}
	}
	var since time.Time
	if raw := q.Get("since"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			since = t
		} else if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			since = time.Now().Add(-d)
		} else {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, "since must be an RFC 3339 time or a duration like 24h", nil)
			return
		}
	}
	limit, cursor := parsePagination(r)
	rows, next, err := h.Fleet.ListHitlAudit(r.Context(), orchestrator.HitlAuditListFilter{
		EnvIDs: envIDs, ActorID: q.Get("actor"), Since: since, Limit: limit, Cursor: cursor,
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := AuditList{Entries: make([]AuditEntry, 0, len(rows))}
	for _, e := range rows {
		out.Entries = append(out.Entries, AuditEntry{
			Seq:            e.Seq,
			Event:          string(e.Event),
			InterventionID: e.InterventionID,
			EnvID:          e.EnvID,
			Actor:          toAPIActor(e.Actor),
			RemoteAddr:     e.RemoteAddr,
			At:             e.At,
			PrevHash:       hex.EncodeToString(e.PrevHash),
			Hash:           hex.EncodeToString(e.Hash),
		})
	}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

func toHitlSpec(s *HitlSpec) *hitl.Spec {
	if s == nil {
		return nil
	}
	out := &hitl.Spec{TimeoutSeconds: s.TimeoutSeconds, OnTimeout: hitl.OnTimeout(s.OnTimeout), RequireActor: s.RequireActor}
	if s.Requests.Ask != nil {
		out.Requests.Ask = &hitl.AskRequests{}
	}
	for _, w := range s.Notify {
		out.Notify = append(out.Notify, hitl.Webhook{URL: w.URL, Secret: w.Secret})
	}
	return out
}

func toAPIActor(a hitl.Actor) Actor {
	return Actor{ID: a.ID, Name: a.Name, Source: string(a.Source), KeyID: a.KeyID}
}

func toAPIIntervention(in hitl.Intervention) Intervention {
	out := Intervention{
		ID:          in.ID,
		EnvID:       in.EnvID,
		Kind:        string(in.Kind),
		State:       string(in.State),
		Payload:     InterventionPayload{Question: in.Payload.Question, Choices: in.Payload.Choices},
		TriggeredBy: toAPIActor(in.TriggeredBy),
		Deadline:    in.Deadline,
		OnTimeout:   string(in.OnTimeout),
		CreatedAt:   in.CreatedAt,
		UpdatedAt:   in.UpdatedAt,
	}
	if d := in.Decision; d != nil {
		out.Decision = &InterventionDecision{
			Outcome:   string(d.Outcome),
			Answer:    d.Answer,
			Reason:    d.Reason,
			DecidedBy: toAPIActor(d.DecidedBy),
			DecidedAt: d.DecidedAt,
		}
	}
	return out
}
