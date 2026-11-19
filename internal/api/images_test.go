package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/orchestrator/orchestratortest"
)

// imageFakeProvider is a fakeProvider whose host can convert images. its
// first full converts answer 507, a host with no room left.
type imageFakeProvider struct {
	*fakeProvider
	images   map[string]orchestrator.Image
	full     int
	converts int
}

func (p *imageFakeProvider) ConvertImage(_ context.Context, ref string) (orchestrator.Image, error) {
	p.converts++
	if p.converts <= p.full {
		return orchestrator.Image{}, &orchestrator.HTTPStatusError{Code: http.StatusInsufficientStorage, Body: "image cache full"}
	}
	if ref == "private/app" {
		return orchestrator.Image{}, &orchestrator.HTTPStatusError{Code: http.StatusBadRequest, Body: "private registries are not supported"}
	}
	return orchestrator.Image{Digest: strings.Repeat("c", 64), Ref: ref, ManifestDigest: "sha256:" + strings.Repeat("d", 64), Name: "oci-c", State: orchestrator.ImageConverting}, nil
}

func (p *imageFakeProvider) GetImage(_ context.Context, digest string) (orchestrator.Image, error) {
	img, ok := p.images[digest]
	if !ok {
		return orchestrator.Image{}, &orchestrator.HTTPStatusError{Code: http.StatusNotFound, Body: "not found"}
	}
	return img, nil
}

func newImageHandler(t *testing.T, images ...orchestrator.Image) http.Handler {
	t.Helper()
	h, _ := newImageHandlerWith(t, images...)
	return h
}

func newImageHandlerWith(t *testing.T, images ...orchestrator.Image) (http.Handler, *imageFakeProvider) {
	t.Helper()
	p := &imageFakeProvider{fakeProvider: newFakeProvider(), images: map[string]orchestrator.Image{}}
	for _, img := range images {
		p.images[img.Digest] = img
	}
	fm, _ := orchestratortest.NewFleet(orchestrator.FleetConfig{Provider: p, Prefix: "fuse-"})
	host := orchestrator.Host{ID: "fc-1", URL: "http://fc-1.test", Capacity: orchestrator.HostCapacity{CPUs: 8, RamMB: 8192, StorageGB: 100, VMCount: 10}}
	if err := fm.RegisterHost(context.Background(), host, p); err != nil {
		t.Fatal(err)
	}
	gpu := orchestrator.Host{ID: "gpu-1", URL: "http://gpu-1.test", Backend: orchestrator.BackendQEMU, Capacity: orchestrator.HostCapacity{CPUs: 8, RamMB: 8192, StorageGB: 100, VMCount: 10}}
	if err := fm.RegisterHost(context.Background(), gpu, newFakeProvider()); err != nil {
		t.Fatal(err)
	}
	return mustRouter(t, &Handler{Fleet: fm}), p
}

// TestConvertImageKeyReusedForAnotherRef: a convert runs as an operation, so
// the same key with a different ref is a 409, not the first ref's image.
func TestConvertImageKeyReusedForAnotherRef(t *testing.T) {
	h, p := newImageHandlerWith(t)
	if rr := doKeyed(t, h, http.MethodPost, "/v1/images", "k-1", ConvertImageRequest{Ref: "ubuntu:24.04"}); rr.Code != http.StatusAccepted {
		t.Fatalf("first: status = %d, body %s", rr.Code, rr.Body.String())
	}
	rr := doKeyed(t, h, http.MethodPost, "/v1/images", "k-1", ConvertImageRequest{Ref: "debian:12"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("other ref: status = %d, want 409. body %s", rr.Code, rr.Body.String())
	}
	if env := decodeError(t, rr.Body); env.Error.Details["reason"] != "idempotency_key_reused" {
		t.Fatalf("reason = %q, want idempotency_key_reused", env.Error.Details["reason"])
	}
	same := doKeyed(t, h, http.MethodPost, "/v1/images", "k-1", ConvertImageRequest{Ref: "ubuntu:24.04"})
	if same.Code != http.StatusAccepted || same.Header().Get(HeaderIdempotentReplayed) != "true" || same.Header().Get(HeaderOperationID) == "" {
		t.Fatalf("same ref: status = %d replayed %q, want a replayed 202 with an operation id", same.Code, same.Header().Get(HeaderIdempotentReplayed))
	}
	if p.converts != 1 {
		t.Fatalf("host converted %d times, want 1", p.converts)
	}
}

// TestConvertImageKeyedFailureRunsAgain: a keyed convert that answered 507
// reaches the host again when retried with the same key, instead of getting
// the durable fake's (and restate's) recorded failure back.
func TestConvertImageKeyedFailureRunsAgain(t *testing.T) {
	h, p := newImageHandlerWith(t)
	p.full = 1
	if rr := doKeyed(t, h, http.MethodPost, "/v1/images", "k-1", ConvertImageRequest{Ref: "ubuntu:24.04"}); rr.Code != http.StatusInsufficientStorage {
		t.Fatalf("first: status = %d, want 507. body %s", rr.Code, rr.Body.String())
	}
	rr := doKeyed(t, h, http.MethodPost, "/v1/images", "k-1", ConvertImageRequest{Ref: "ubuntu:24.04"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("retry: status = %d, want 202. body %s", rr.Code, rr.Body.String())
	}
	if p.converts != 2 {
		t.Fatalf("host converted %d times, want 2", p.converts)
	}
}

func TestConvertImage(t *testing.T) {
	h := newImageHandler(t)
	rr := doJSON(t, h, http.MethodPost, "/v1/images", ConvertImageRequest{Ref: "ubuntu:24.04"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body: %s", rr.Code, rr.Body.String())
	}
	var img Image
	if err := json.NewDecoder(rr.Body).Decode(&img); err != nil {
		t.Fatal(err)
	}
	if img.Digest != strings.Repeat("c", 64) || img.State != "converting" || img.HostID != "fc-1" || img.Ref != "ubuntu:24.04" {
		t.Fatalf("image = %+v", img)
	}
}

func TestConvertImageRejections(t *testing.T) {
	h := newImageHandler(t)
	for _, tc := range []struct {
		name string
		req  ConvertImageRequest
		want int
	}{
		{"no ref", ConvertImageRequest{}, http.StatusBadRequest},
		{"bad ref", ConvertImageRequest{Ref: "-oops"}, http.StatusBadRequest},
		{"bad label", ConvertImageRequest{Ref: "ubuntu", Labels: map[string]string{"a": "b c"}}, http.StatusBadRequest},
		{"qemu host", ConvertImageRequest{Ref: "ubuntu", HostID: "gpu-1"}, http.StatusBadRequest},
		{"unknown host", ConvertImageRequest{Ref: "ubuntu", HostID: "nope"}, http.StatusNotFound},
		{"no host in the region", ConvertImageRequest{Ref: "ubuntu", Region: "eu-west"}, http.StatusServiceUnavailable},
		{"agent refuses the ref", ConvertImageRequest{Ref: "private/app"}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doJSON(t, h, http.MethodPost, "/v1/images", tc.req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d. body: %s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestGetImage(t *testing.T) {
	d := strings.Repeat("a", 64)
	h := newImageHandler(t, orchestrator.Image{Digest: d, State: orchestrator.ImageFailed, Error: "fc-bake-distro-rootfs: unsupported distro"})

	rr := doJSON(t, h, http.MethodGet, "/v1/images/"+d, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}
	var img Image
	if err := json.NewDecoder(rr.Body).Decode(&img); err != nil {
		t.Fatal(err)
	}
	if img.State != "failed" || !strings.Contains(img.Error, "unsupported distro") || img.HostID != "fc-1" {
		t.Fatalf("image = %+v", img)
	}
	if rr := doJSON(t, h, http.MethodGet, "/v1/images/"+strings.Repeat("b", 64), nil); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown digest: status = %d, want 404", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodGet, "/v1/images/nothex", nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad digest: status = %d, want 400", rr.Code)
	}
}

func TestCreateWithImageDigest(t *testing.T) {
	ready, converting := strings.Repeat("a", 64), strings.Repeat("b", 64)
	h := newImageHandler(t,
		orchestrator.Image{Digest: ready, State: orchestrator.ImageReady, Name: "oci-a"},
		orchestrator.Image{Digest: converting, State: orchestrator.ImageConverting})

	create := func(taskID string, spec ResourceSpec, desktop *DesktopSpec, seed string) int {
		rr := doJSON(t, h, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
			TaskID: taskID, Spec: spec, ManifestInline: encodeManifest(t), Desktop: desktop, SeedSnapshotID: seed,
		})
		return rr.Code
	}
	base := ResourceSpec{CPUs: 1, RamMB: 256}
	with := func(f func(*ResourceSpec)) ResourceSpec { s := base; f(&s); return s }

	if got := create("ok", with(func(s *ResourceSpec) { s.ImageDigest = ready }), nil, ""); got != http.StatusCreated {
		t.Fatalf("ready digest: status = %d, want 201", got)
	}
	for _, tc := range []struct {
		name    string
		spec    ResourceSpec
		desktop *DesktopSpec
		seed    string
		want    int
	}{
		{"converting", with(func(s *ResourceSpec) { s.ImageDigest = converting }), nil, "", http.StatusConflict},
		{"unknown", with(func(s *ResourceSpec) { s.ImageDigest = strings.Repeat("e", 64) }), nil, "", http.StatusConflict},
		{"malformed", with(func(s *ResourceSpec) { s.ImageDigest = "sha256:abc" }), nil, "", http.StatusBadRequest},
		{"with image", with(func(s *ResourceSpec) { s.ImageDigest = ready; s.Image = "base" }), nil, "", http.StatusBadRequest},
		{"with seed", with(func(s *ResourceSpec) { s.ImageDigest = ready }), nil, "snap-1", http.StatusBadRequest},
		{"with gpu", with(func(s *ResourceSpec) { s.ImageDigest = ready; s.GPUs = 1 }), nil, "", http.StatusBadRequest},
		{"with desktop", with(func(s *ResourceSpec) { s.ImageDigest = ready }), &DesktopSpec{Width: 1024, Height: 768}, "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := create("t-"+strings.ReplaceAll(tc.name, " ", "-"), tc.spec, tc.desktop, tc.seed); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestImageSentinels: image errors a caller can cause are 4xx, never 500.
func TestImageSentinels(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{orchestrator.ErrInvalidImage, http.StatusBadRequest},
		{orchestrator.ErrImageUnsupported, http.StatusBadRequest},
		{orchestrator.ErrImageNotFound, http.StatusNotFound},
		{orchestrator.ErrImageNotReady, http.StatusConflict},
		{orchestrator.ErrImageHostFailed, http.StatusServiceUnavailable},
		{orchestrator.ErrImageCacheFull, http.StatusInsufficientStorage},
	} {
		if got, _ := classifyFleetError(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
			t.Errorf("%v: status %d, want %d", tc.err, got, tc.want)
		}
	}
}

// keyCapturingImages records the idempotency key each durable convert saw.
type keyCapturingImages struct{ keys []string }

func (d *keyCapturingImages) ConvertImage(ctx context.Context, req orchestrator.ConvertImageRequest) (orchestrator.Image, error) {
	d.keys = append(d.keys, orchestrator.IdempotencyKey(ctx))
	return orchestrator.Image{Digest: strings.Repeat("c", 64), Ref: req.Ref, HostID: "fc-1", State: orchestrator.ImageConverting}, nil
}

// TestConvertImageForwardsTheIdempotencyKey: the caller's key reaches the
// durable client, so restate attaches a retry to the first convert.
func TestConvertImageForwardsTheIdempotencyKey(t *testing.T) {
	fm, _ := orchestratortest.NewFleet(orchestrator.FleetConfig{Provider: newFakeProvider(), Prefix: "fuse-"})
	d := &keyCapturingImages{}
	fm.SetDurableImages(d)
	h := mustRouter(t, &Handler{Fleet: fm})

	for _, key := range []string{"k-1", ""} {
		req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"ref":"ubuntu:24.04"}`))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set(HeaderIdempotencyKey, key)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("key %q: status = %d, body %s", key, rr.Code, rr.Body.String())
		}
	}
	if len(d.keys) != 2 || d.keys[0] != "master:image.convert:k-1" || d.keys[1] != "" {
		t.Fatalf("keys = %q, want [master:image.convert:k-1, \"\"]", d.keys)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"ref":"ubuntu:24.04"}`))
	req.Header.Set(HeaderIdempotencyKey, "bad key\n")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || len(d.keys) != 2 {
		t.Fatalf("invalid key: status = %d, converts %d", rr.Code, len(d.keys))
	}
}
