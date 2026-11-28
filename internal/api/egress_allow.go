package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// setEgressAllow replaces a running direct environment's outbound
// allowlist and answers 200 with its egress status. an empty list lifts the
// restriction. it runs as an operation, so an Idempotency-Key replays the
// first answer.
//
//	@Summary	Replace egress allowlist
//	@Tags		environments
//	@Accept		json
//	@Produce	json
//	@Param		vmId	path		string					true	"VM identifier"
//	@Param		body	body		SetEgressAllowRequest	true	"New allowlist"
//	@Success	200		{object}	EgressStatus
//	@Failure	400		{object}	Error
//	@Failure	404		{object}	Error
//	@Failure	409		{object}	Error
//	@Failure	501		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/environments/{vmId}/egress/allow [put]
func (h *Handler) setEgressAllow(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	// decoded raw first: a body without the field is refused, where null
	// or [] lifts the list.
	var raw struct {
		Allow json.RawMessage `json:"allow"`
	}
	if !decodeJSON(w, r, MaxJSONBodyBytes, &raw) {
		return
	}
	if len(raw.Allow) == 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"allow is required; send [] to lift the allowlist", nil)
		return
	}
	var req SetEgressAllowRequest
	if err := json.Unmarshal(raw.Allow, &req.Allow); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"allow must be a list of ipv4 cidrs", nil)
		return
	}
	info, err := h.Fleet.SetEgressAllow(r.Context(), vmID, req.Allow)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIEgress(info.Egress, info.Spec.Egress.Allow))
}
