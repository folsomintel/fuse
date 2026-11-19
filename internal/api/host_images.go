package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// getHostImage reports one host's record of an image.
//
//	@Summary		Get an image on a host
//	@Description	Reports the image's conversion state on this host only: converting, ready or failed (with error). GET /v1/images/{digest} answers with the best record of any host, so a convert pinned to a host polls this route instead; another host's ready record says nothing about the pinned one.
//	@Tags			images
//	@Produce		json
//	@Param			hostId	path		string	true	"Host identifier"
//	@Param			digest	path		string	true	"Image digest (hex sha256) from POST /v1/images"
//	@Success		200		{object}	Image
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		503		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/hosts/{hostId}/images/{digest} [get]
func (h *Handler) getHostImage(w http.ResponseWriter, r *http.Request) {
	img, err := h.Fleet.GetHostImage(r.Context(), chi.URLParam(r, "hostId"), chi.URLParam(r, "digest"))
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIImage(img))
}
