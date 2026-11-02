package api

import (
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
		Kind:    kind,
		Ask:     hitl.AskPayload{Question: req.Question, Choices: req.Choices},
		Timeout: time.Duration(req.TimeoutSeconds) * time.Second,
		By:      actorFromRequest(r),
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
//	@Param		limit	query		int		false	"Max results per page (default 50, max 200)"
//	@Param		cursor	query		string	false	"Opaque pagination cursor from a previous response's next_cursor"
//	@Success	200		{object}	InterventionList
//	@Failure	400		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/interventions [get]
func (h *Handler) listInterventions(w http.ResponseWriter, r *http.Request) {
	limit, cursor := parsePagination(r)
	state := hitl.State(r.URL.Query().Get("state"))
	if state != "" && !state.Valid() {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "unknown state \""+string(state)+"\"", nil)
		return
	}
	list, next, err := h.Fleet.ListInterventions(r.Context(), orchestrator.InterventionListFilter{
		EnvID:  r.URL.Query().Get("env"),
		State:  state,
		Limit:  limit,
		Cursor: cursor,
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
	id := chi.URLParam(r, "interventionId")
	wait, ok := parseWait(r.URL.Query().Get("wait"))
	if !ok {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "wait must be seconds or a duration like 30s", nil)
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

// interventionAction decides an intervention: ?action=approve or reject.
// deciding is master-token only until scoped keys land, so an agent holding
// an ordinary api key cannot answer its own request.
//
//	@Summary		Decide intervention
//	@Description	Approve (with an answer, for an ask) or reject (with a reason) a pending intervention. Requires the master token.
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
	if !h.requireMasterToken(w, r, "deciding an intervention requires the master token") {
		return
	}
	var req DecideInterventionRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
			return
		}
	}
	in, err := h.Fleet.DecideIntervention(r.Context(), chi.URLParam(r, "interventionId"),
		outcome, req.Answer, req.Reason, actorFromRequest(r))
	if err != nil {
		writeFleetError(w, err)
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

// actorFromRequest labels the caller from how it authenticated. the master
// token (and insecure mode) is "master"; an api key is "key:<id>".
func actorFromRequest(r *http.Request) hitl.Actor {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.Master {
		return hitl.Actor{ID: "master", Source: hitl.ActorKey}
	}
	return hitl.Actor{ID: "key:" + p.KeyID, Source: hitl.ActorKey, KeyID: p.KeyID}
}

func toHitlSpec(s *HitlSpec) *hitl.Spec {
	if s == nil {
		return nil
	}
	out := &hitl.Spec{TimeoutSeconds: s.TimeoutSeconds, OnTimeout: hitl.OnTimeout(s.OnTimeout)}
	if s.Requests.Ask != nil {
		out.Requests.Ask = &hitl.AskRequests{}
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
