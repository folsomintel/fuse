package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/folsomintel/fuse/internal/fusefile"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// ConvertImageRequest asks for an oci image to be converted to a rootfs on a
// firecracker host. HostID, Region and Labels pick the host the same way a
// create's placement does, so a create placed the same way can boot the
// result.
type ConvertImageRequest struct {
	Ref    string            `json:"ref"`
	HostID string            `json:"host_id,omitempty"`
	Region string            `json:"region,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Image is an oci image converted, or being converted, on a host.
//
// Digest is what a create names in spec.image_digest. It is the hex sha256
// of the image's manifest digest for the host's arch together with the
// digest of the fused binary baked into the rootfs, so a moved tag or a new
// fused build is a new image. State is converting, ready or failed; Error is
// set when it failed, and converting the same ref again retries.
type Image struct {
	Digest         string `json:"digest"`
	Ref            string `json:"ref"`
	ManifestDigest string `json:"manifest_digest"`
	HostID         string `json:"host_id"`
	State          string `json:"state"`
	Error          string `json:"error,omitempty"`
}

func toAPIImage(img orchestrator.Image) Image {
	return Image{
		Digest:         img.Digest,
		Ref:            img.Ref,
		ManifestDigest: img.ManifestDigest,
		HostID:         img.HostID,
		State:          string(img.State),
		Error:          img.Error,
	}
}

// convertImage starts converting an oci image.
//
//	@Summary		Convert an oci image
//	@Description	Resolves ref to its manifest for a firecracker host's arch and starts converting it to a rootfs in the background. Answers 202 with the image's digest and state; poll GET /v1/images/{digest} until it is ready, then create with spec.image_digest. Converting a ref that is already converting or ready answers with that record; one that failed is retried. Public registries only, apt and dnf based images only; qemu hosts cannot convert. An Idempotency-Key makes a retry attach to the first convert instead of starting another.
//	@Tags			images
//	@Accept			json
//	@Produce		json
//	@Param			Idempotency-Key	header	string	false	"Dedupes a retried convert"
//	@Param			body	body		ConvertImageRequest	true	"Image to convert"
//	@Success		202		{object}	Image
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		503		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/images [post]
func (h *Handler) convertImage(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(HeaderIdempotencyKey)
	if key != "" && !validIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			HeaderIdempotencyKey+": want 1 to 255 visible ascii characters", nil)
		return
	}
	var req ConvertImageRequest
	if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}
	if req.Ref == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "ref is required", nil)
		return
	}
	if !fusefile.ValidImageRef(req.Ref) {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			fmt.Sprintf("ref: invalid oci image reference %q (e.g. ubuntu:24.04 or ghcr.io/org/app@sha256:<digest>)", req.Ref), nil)
		return
	}
	if err := validatePlacementSpec(ResourceSpec{Labels: req.Labels}); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	// the key rides the context to the durable client, so a retry after a
	// lost response attaches to the first Images.Convert instead of starting
	// another.
	ctx := r.Context()
	if key != "" {
		ctx = orchestrator.WithIdempotencyKey(ctx, operationOwner(principal(r))+":image.convert:"+key)
	}
	img, err := h.Fleet.ConvertImage(ctx, orchestrator.ConvertImageRequest{
		Ref:    req.Ref,
		HostID: req.HostID,
		Region: req.Region,
		Labels: copyLabels(req.Labels),
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toAPIImage(img))
}

// getImage reports a converted image.
//
//	@Summary		Get an image
//	@Description	Reports an image's conversion state: converting, ready or failed (with error). When more than one host holds the digest, the most usable record wins (ready, then converting, then failed).
//	@Tags			images
//	@Produce		json
//	@Param			digest	path		string	true	"Image digest (hex sha256) from POST /v1/images"
//	@Success		200		{object}	Image
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/images/{digest} [get]
func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	img, err := h.Fleet.GetImage(r.Context(), chi.URLParam(r, "digest"))
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIImage(img))
}
