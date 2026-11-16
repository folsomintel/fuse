package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// imageFakeProvider is a fakeProvider whose host can convert images.
type imageFakeProvider struct {
	*fakeProvider
	images map[string]orchestrator.Image
}

func (p *imageFakeProvider) ConvertImage(_ context.Context, ref string) (orchestrator.Image, error) {
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
	p := &imageFakeProvider{fakeProvider: newFakeProvider(), images: map[string]orchestrator.Image{}}
	for _, img := range images {
		p.images[img.Digest] = img
	}
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{Provider: p, Prefix: "fuse-"})
	host := orchestrator.Host{ID: "fc-1", URL: "http://fc-1.test", Capacity: orchestrator.HostCapacity{CPUs: 8, RamMB: 8192, StorageGB: 100, VMCount: 10}}
	if err := fm.RegisterHost(context.Background(), host, p); err != nil {
		t.Fatal(err)
	}
	gpu := orchestrator.Host{ID: "gpu-1", URL: "http://gpu-1.test", Backend: orchestrator.BackendQEMU, Capacity: orchestrator.HostCapacity{CPUs: 8, RamMB: 8192, StorageGB: 100, VMCount: 10}}
	if err := fm.RegisterHost(context.Background(), gpu, newFakeProvider()); err != nil {
		t.Fatal(err)
	}
	return mustRouter(t, &Handler{Fleet: fm})
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
	} {
		if got, _ := classifyFleetError(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
			t.Errorf("%v: status %d, want %d", tc.err, got, tc.want)
		}
	}
}
