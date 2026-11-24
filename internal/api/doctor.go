package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// environmentDoctor relays the guest's self-check report.
//
// A guest agent baked before /v1/doctor existed answers its mux's 404. That
// is an image older than the feature, not a missing environment, so it is
// reported as 501 with the fix rather than relayed as an internal fault.
func (h *Handler) environmentDoctor(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	res, err := h.Fleet.GuestDoctor(r.Context(), vmID)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	if res.Status == http.StatusNotFound {
		writeError(w, http.StatusNotImplemented, CodeUnimplemented,
			"the guest agent in this environment predates doctor; rebuild the image on a newer fused", nil)
		return
	}
	relayGuestComputer(w, res.Status, res.Body)
}
