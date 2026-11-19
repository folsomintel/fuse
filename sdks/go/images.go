package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Image states.
const (
	ImageConverting = "converting"
	ImageReady      = "ready"
	ImageFailed     = "failed"
)

// ConvertImageRequest asks for an oci image to be converted to a rootfs on a
// firecracker host. HostID, Region and Labels pick the host the way a
// create's placement does.
type ConvertImageRequest struct {
	Ref    string            `json:"ref"`
	HostID string            `json:"host_id,omitempty"`
	Region string            `json:"region,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Image is an oci image converted, or being converted, on a host. Digest is
// what a create names in Spec.ImageDigest once State is ImageReady; Error
// says why when it is ImageFailed.
type Image struct {
	Digest         string `json:"digest"`
	Ref            string `json:"ref"`
	ManifestDigest string `json:"manifest_digest"`
	HostID         string `json:"host_id"`
	State          string `json:"state"`
	Error          string `json:"error,omitempty"`
}

// ImagesService converts oci images (a Fusefile's `from:`) into bootable
// rootfs images.
type ImagesService struct {
	t *transport
}

// Convert starts converting req.Ref and returns the image at once, usually
// still converting. Poll Get until it is ready. Converting a ref that is
// already converting or ready returns that image; one that failed is retried.
// The convert runs as an operation: the same WithIdempotencyKey makes a retry
// attach to the first convert, and one the server answers with a 202
// operation is followed until it finishes.
func (s *ImagesService) Convert(ctx context.Context, req ConvertImageRequest, opts ...CallOption) (*Image, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("images service is not configured")
	}
	if req.Ref == "" {
		return nil, errors.New("image ref is required")
	}
	hreq, err := s.t.newRequest(ctx, http.MethodPost, "/v1/images", nil, req)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.doOperation(ctx, hreq, opts)
	if err != nil {
		return nil, err
	}
	return decodeImage(resp)
}

// Get reports an image's conversion state by digest.
func (s *ImagesService) Get(ctx context.Context, digest string) (*Image, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("images service is not configured")
	}
	if digest == "" {
		return nil, errors.New("image digest is required")
	}
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/images/"+url.PathEscape(digest), nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return nil, err
	}
	return decodeImage(resp)
}

func decodeImage(resp *http.Response) (*Image, error) {
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var img Image
	if err := json.NewDecoder(resp.Body).Decode(&img); err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return &img, nil
}
