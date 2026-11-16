package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// durableImagesStub stands in for the restate client.
type durableImagesStub struct {
	reqs []ConvertImageRequest
}

func (d *durableImagesStub) ConvertImage(_ context.Context, req ConvertImageRequest) (Image, error) {
	d.reqs = append(d.reqs, req)
	return Image{Digest: digestOf("d"), Ref: req.Ref, HostID: "fc-1", State: ImageConverting}, nil
}

func TestConvertImageGoesThroughRestateWhenSet(t *testing.T) {
	fc := newImageProvider()
	fm := imageFleet(t, map[string]Provider{"fc-1": fc}, nil)
	d := &durableImagesStub{}
	fm.SetDurableImages(d)

	img, err := fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: "ubuntu", HostID: "fc-1"})
	if err != nil {
		t.Fatalf("ConvertImage: %v", err)
	}
	if img.Digest != digestOf("d") || len(d.reqs) != 1 || d.reqs[0].HostID != "fc-1" {
		t.Fatalf("image %+v, durable requests %+v", img, d.reqs)
	}
	if len(fc.converted) != 0 {
		t.Fatalf("the host was called in process: %v", fc.converted)
	}

	// mode off: the host is called directly.
	fm.SetDurableImages(nil)
	if _, err := fm.ConvertImage(context.Background(), ConvertImageRequest{Ref: "ubuntu"}); err != nil {
		t.Fatalf("ConvertImage in process: %v", err)
	}
	if len(fc.converted) != 1 || len(d.reqs) != 1 {
		t.Fatalf("in process: host converted %v, durable requests %d", fc.converted, len(d.reqs))
	}
}

func TestImagePoll(t *testing.T) {
	d := digestOf("a")
	fc := newImageProvider(Image{Digest: d, State: ImageFailed, Error: "boom"})
	fm := imageFleet(t, map[string]Provider{"fc-1": fc}, nil)
	watched := Image{Digest: d, Ref: "ubuntu", HostID: "fc-1", State: ImageConverting}

	got, err := fm.ImagePoll(context.Background(), watched)
	if err != nil || got.State != ImageFailed || got.Error != "boom" || got.Ref != "ubuntu" {
		t.Fatalf("poll = %+v, %v; want the host's failed record", got, err)
	}

	// a host that cannot be asked counts as still converting.
	fc.getErr = errors.New("connection refused")
	got, err = fm.ImagePoll(context.Background(), watched)
	if err != nil || got.State != ImageConverting {
		t.Fatalf("unreachable host: poll = %+v, %v; want converting", got, err)
	}

	// a host with no record, or no host at all, ends the watch failed.
	fc.getErr = nil
	got, _ = fm.ImagePoll(context.Background(), Image{Digest: digestOf("b"), HostID: "fc-1", State: ImageConverting})
	if got.State != ImageFailed || !strings.Contains(got.Error, "no longer has a record") {
		t.Fatalf("missing record: poll = %+v", got)
	}
	got, _ = fm.ImagePoll(context.Background(), Image{Digest: d, HostID: "gone", State: ImageConverting})
	if got.State != ImageFailed || !strings.Contains(got.Error, "no longer registered") {
		t.Fatalf("missing host: poll = %+v", got)
	}
}
