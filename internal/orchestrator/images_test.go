package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// imageProvider is a firecracker-like host that holds converted images.
type imageProvider struct {
	*buildProvider
	mu         sync.Mutex
	images     map[string]Image
	convertErr error
	createErr  error
	getErr     error
	converted  []string
	created    []Spec
}

func newImageProvider(images ...Image) *imageProvider {
	p := &imageProvider{buildProvider: &buildProvider{}, images: map[string]Image{}}
	for _, img := range images {
		p.images[img.Digest] = img
	}
	return p
}

func (p *imageProvider) Create(ctx context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	p.created = append(p.created, spec)
	err := p.createErr
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return p.buildProvider.Create(ctx, spec)
}

func (p *imageProvider) ConvertImage(_ context.Context, ref string) (Image, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.convertErr != nil {
		return Image{}, p.convertErr
	}
	p.converted = append(p.converted, ref)
	return Image{Digest: digestOf("c"), Ref: ref, Name: "oci-c", State: ImageConverting}, nil
}

func (p *imageProvider) GetImage(_ context.Context, digest string) (Image, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.getErr != nil {
		return Image{}, p.getErr
	}
	img, ok := p.images[digest]
	if !ok {
		return Image{}, &HTTPStatusError{Code: http.StatusNotFound, Body: "image not found"}
	}
	return img, nil
}

func digestOf(c string) string { return strings.Repeat(c, 64) }

func imageFleet(t *testing.T, hosts map[string]Provider, backends map[string]HostBackend) *FleetManager {
	t.Helper()
	fm := newInlineFleet(FleetConfig{Provider: &buildProvider{}, Prefix: "fuse-"})
	for id, p := range hosts {
		h := layerHost(id, "amd64")
		if b, ok := backends[id]; ok {
			h.Backend = b
		}
		if err := fm.RegisterHost(context.Background(), h, p); err != nil {
			t.Fatal(err)
		}
	}
	return fm
}

func TestConvertImageRunsOnAFirecrackerHost(t *testing.T) {
	fc := newImageProvider()
	fm := imageFleet(t, map[string]Provider{"fc-1": fc, "gpu-1": newImageProvider()},
		map[string]HostBackend{"gpu-1": BackendQEMU})

	img, err := fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: "ubuntu:24.04"})
	if err != nil {
		t.Fatalf("ConvertImage: %v", err)
	}
	if img.HostID != "fc-1" || img.State != ImageConverting {
		t.Fatalf("image = %+v, want converting on fc-1", img)
	}
	if len(fc.converted) != 1 {
		t.Fatalf("fc-1 converted %v, want one ref", fc.converted)
	}

	// a qemu host is never a convert target, pinned or not.
	_, err = fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: "ubuntu:24.04", HostID: "gpu-1"})
	if !errors.Is(err, ErrImageUnsupported) {
		t.Fatalf("convert pinned to qemu: err = %v, want ErrImageUnsupported", err)
	}
	_, err = fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: "ubuntu:24.04", HostID: "nope"})
	if !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("convert pinned to unknown host: err = %v, want ErrHostNotFound", err)
	}
}

// a create with spec.region only lands in that region, so the conversion it
// boots from has to run there too, whatever host the ref hashes onto.
func TestConvertImageStaysInTheRegion(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: &buildProvider{}, Prefix: "fuse-"})
	for _, h := range []struct{ id, region string }{{"fc-a", "us-east"}, {"fc-b", "us-east"}, {"fc-c", "eu-west"}} {
		host := layerHost(h.id, "amd64")
		host.Region = h.region
		if err := fm.RegisterHost(context.Background(), host, newImageProvider()); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"ubuntu", "ubuntu:24.04", "debian:12", "fedora:40", "org/app", "ghcr.io/org/app"} {
		img, err := fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: ref, Region: "eu-west"})
		if err != nil {
			t.Fatalf("ConvertImage(%s): %v", ref, err)
		}
		if img.HostID != "fc-c" {
			t.Fatalf("ConvertImage(%s) in eu-west ran on %s, want fc-c", ref, img.HostID)
		}
	}
	_, err := fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: "ubuntu", Region: "ap-south"})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("convert in a region with no host: err = %v, want ErrNoCapacity", err)
	}
}

func TestConvertImageMapsAgentErrors(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{http.StatusBadRequest, ErrInvalidImage},
		{http.StatusNotFound, ErrImageNotFound},
		{http.StatusNotImplemented, ErrImageUnsupported},
		{http.StatusInsufficientStorage, ErrImageCacheFull},
		{http.StatusBadGateway, ErrImageHostFailed},
	}
	for _, tc := range cases {
		fc := newImageProvider()
		fc.convertErr = &HTTPStatusError{Code: tc.code, Body: "nope"}
		fm := imageFleet(t, map[string]Provider{"fc-1": fc}, nil)
		_, err := fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: "ubuntu"})
		if !errors.Is(err, tc.want) {
			t.Errorf("agent %d: err = %v, want %v", tc.code, err, tc.want)
		}
	}
}

func TestGetImagePrefersReady(t *testing.T) {
	d := digestOf("a")
	fm := imageFleet(t, map[string]Provider{
		"fc-1": newImageProvider(Image{Digest: d, State: ImageFailed, Error: "boom"}),
		"fc-2": newImageProvider(Image{Digest: d, State: ImageReady, Name: "oci-a"}),
		"fc-3": newImageProvider(),
	}, nil)

	img, err := fm.GetImage(context.Background(), d)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if img.HostID != "fc-2" || img.State != ImageReady {
		t.Fatalf("image = %+v, want ready on fc-2", img)
	}
	if _, err := fm.GetImage(context.Background(), digestOf("b")); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("unknown digest: err = %v, want ErrImageNotFound", err)
	}
	if _, err := fm.GetImage(context.Background(), "sha256:x"); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("bad digest: err = %v, want ErrInvalidImage", err)
	}
}

// TestGetHostImage: a host-scoped read answers with that host's own record,
// not the best one in the fleet, so a convert pinned to a host that is still
// converting does not read as ready because another host has it.
func TestGetHostImage(t *testing.T) {
	d := digestOf("a")
	fc2 := newImageProvider(Image{Digest: d, State: ImageConverting})
	fm := imageFleet(t, map[string]Provider{
		"fc-1":   newImageProvider(Image{Digest: d, State: ImageReady, Name: "oci-a"}),
		"fc-2":   fc2,
		"qemu-1": newImageProvider(),
	}, map[string]HostBackend{"qemu-1": BackendQEMU})
	ctx := context.Background()

	if img, err := fm.GetImage(ctx, d); err != nil || img.HostID != "fc-1" || img.State != ImageReady {
		t.Fatalf("GetImage = %+v, %v; want ready on fc-1", img, err)
	}
	img, err := fm.GetHostImage(ctx, "fc-2", d)
	if err != nil || img.HostID != "fc-2" || img.State != ImageConverting {
		t.Fatalf("GetHostImage(fc-2) = %+v, %v; want converting on fc-2", img, err)
	}
	if err := fm.CordonHost(ctx, "fc-2"); err != nil {
		t.Fatal(err)
	}
	if img, err := fm.GetHostImage(ctx, "fc-2", d); err != nil || img.State != ImageConverting {
		t.Fatalf("GetHostImage on a cordoned host = %+v, %v; want its record", img, err)
	}

	for _, c := range []struct {
		host, digest string
		want         error
	}{
		{"fc-1", digestOf("b"), ErrImageNotFound},
		{"fc-1", "sha256:x", ErrInvalidImage},
		{"gone", d, ErrHostNotFound},
		{"qemu-1", d, ErrImageUnsupported},
	} {
		if _, err := fm.GetHostImage(ctx, c.host, c.digest); !errors.Is(err, c.want) {
			t.Fatalf("GetHostImage(%s, %s) = %v, want %v", c.host, c.digest, err, c.want)
		}
	}
	fc2.getErr = errors.New("connection refused")
	if _, err := fm.GetHostImage(ctx, "fc-2", d); !errors.Is(err, ErrImageHostFailed) {
		t.Fatalf("unreachable host: err = %v, want ErrImageHostFailed", err)
	}
}

// create boots the converted rootfs on the host it is ready on, and refuses
// a digest that is not ready anywhere it could land.
func TestCreateFromImageDigest(t *testing.T) {
	ready, converting := digestOf("a"), digestOf("b")
	fc1 := newImageProvider(Image{Digest: converting, State: ImageConverting})
	fc2 := newImageProvider(Image{Digest: ready, State: ImageReady, Name: "oci-aaaa"})
	fm := imageFleet(t, map[string]Provider{"fc-1": fc1, "fc-2": fc2}, nil)
	ctx := context.Background()

	info, err := fm.ProvisionAndAssign(ctx, "t1", Spec{CPUs: 1, RamMB: 256, ImageDigest: ready}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.HostID != "fc-2" {
		t.Fatalf("host = %s, want fc-2 (where the image is ready)", info.HostID)
	}
	if len(fc2.created) != 1 || fc2.created[0].Image != "oci-aaaa" {
		t.Fatalf("fc-2 creates = %+v, want one booting oci-aaaa", fc2.created)
	}

	_, err = fm.ProvisionAndAssign(ctx, "t2", Spec{CPUs: 1, RamMB: 256, ImageDigest: converting}, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(err, ErrImageNotReady) || !strings.Contains(err.Error(), "converting on host fc-1") {
		t.Fatalf("converting digest: err = %v, want ErrImageNotReady naming fc-1", err)
	}
	_, err = fm.ProvisionAndAssign(ctx, "t3", Spec{CPUs: 1, RamMB: 256, ImageDigest: ready, HostID: "fc-1"}, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(err, ErrImageNotReady) {
		t.Fatalf("digest pinned to a host without it: err = %v, want ErrImageNotReady", err)
	}
	if _, ok := fm.GetVMByTask("t2"); ok {
		t.Fatal("a refused create left a vm behind")
	}
}

// an image evicted between resolveImage and the agent's create is not
// ready, the same 409 as one that was never converted.
func TestCreateFromAnEvictedImageIsNotReady(t *testing.T) {
	d := digestOf("a")
	fc := newImageProvider(Image{Digest: d, State: ImageReady, Name: "oci-aaaa"})
	fc.createErr = &HTTPStatusError{Code: http.StatusNotFound, Body: "image 'oci-aaaa' is no longer converted on this host"}
	fm := imageFleet(t, map[string]Provider{"fc-1": fc}, nil)

	_, err := fm.ProvisionAndAssign(context.Background(), "t1", Spec{CPUs: 1, RamMB: 256, ImageDigest: d}, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(err, ErrImageNotReady) {
		t.Fatalf("evicted image: err = %v, want ErrImageNotReady", err)
	}
}

func TestScheduleKeepsImageDigestOffQemu(t *testing.T) {
	q := layerHost("q", "amd64")
	q.Backend = BackendQEMU
	q.State = HostActive
	if _, _, err := Schedule(Spec{CPUs: 1, RamMB: 256, ImageDigest: digestOf("a")}, []*Host{&q}, PlacementSpread); err == nil {
		t.Fatal("scheduled an oci image onto a qemu host")
	}
}
