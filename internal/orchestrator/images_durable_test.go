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

// TestImageHost: the place step picks the host ImageConvert would, and the
// convert pinned to it lands there.
func TestImageHost(t *testing.T) {
	fm := imageFleet(t, map[string]Provider{"fc-1": newImageProvider(), "fc-2": newImageProvider()}, nil)
	req := ConvertImageRequest{Ref: "ubuntu"}
	hostID, err := fm.ImageHost(context.Background(), req)
	if err != nil {
		t.Fatalf("ImageHost: %v", err)
	}
	img, err := fm.ImageConvert(context.Background(), req)
	if err != nil || img.HostID != hostID {
		t.Fatalf("ImageConvert = %+v, %v; want host %s", img, err, hostID)
	}
	req.HostID = hostID
	if img, err := fm.ImageConvert(context.Background(), req); err != nil || img.HostID != hostID {
		t.Fatalf("pinned ImageConvert = %+v, %v; want host %s", img, err, hostID)
	}
	if _, err := fm.ImageHost(context.Background(), ConvertImageRequest{Ref: "ubuntu", HostID: "gone"}); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("ImageHost on an unknown host: %v, want ErrHostNotFound", err)
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

// TestImagePollSupersededAttempt: a record from a later attempt ends the
// earlier attempt's watch failed instead of following the retry.
func TestImagePollSupersededAttempt(t *testing.T) {
	d := digestOf("a")
	fc := newImageProvider(Image{Digest: d, State: ImageConverting, CreatedAt: "2026-11-16T10:05:00Z"})
	fm := imageFleet(t, map[string]Provider{"fc-1": fc}, nil)

	old := Image{Digest: d, HostID: "fc-1", State: ImageConverting, CreatedAt: "2026-11-16T10:00:00Z"}
	got, err := fm.ImagePoll(context.Background(), old)
	if err != nil || got.State != ImageFailed || got.CreatedAt != old.CreatedAt || !strings.Contains(got.Error, "retried") {
		t.Fatalf("superseded poll = %+v, %v; want the old attempt failed", got, err)
	}

	// the current attempt's watch, and one without an attempt, follow the record.
	for _, w := range []Image{{Digest: d, HostID: "fc-1", State: ImageConverting, CreatedAt: "2026-11-16T10:05:00Z"}, {Digest: d, HostID: "fc-1", State: ImageConverting}} {
		if got, err := fm.ImagePoll(context.Background(), w); err != nil || got.State != ImageConverting {
			t.Fatalf("poll of %+v = %+v, %v; want converting", w, got, err)
		}
	}
}

// failEventStore refuses every event write.
type failEventStore struct{ *MemoryStateStore }

func (failEventStore) AppendEvent(context.Context, EventRecord) error {
	return errors.New("db down")
}

// TestImageConvertDoneReturnsAFailedEventWrite: the done step has to fail
// when its event does not land, so restate retries it instead of journaling
// an outcome nothing recorded.
func TestImageConvertDoneReturnsAFailedEventWrite(t *testing.T) {
	fm := NewFleetManager(FleetConfig{Provider: &buildProvider{}, Prefix: "fuse-", StateStore: failEventStore{NewMemoryStateStore()}})
	img := Image{Digest: digestOf("d"), Ref: "ubuntu", HostID: "fc-1", State: ImageReady}
	if err := fm.ImageConvertDone(context.Background(), img, false); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("ImageConvertDone = %v, want the event write error", err)
	}

	fm = NewFleetManager(FleetConfig{Provider: &buildProvider{}, Prefix: "fuse-", StateStore: NewMemoryStateStore()})
	if err := fm.ImageConvertDone(context.Background(), img, false); err != nil {
		t.Fatalf("ImageConvertDone with a working store: %v", err)
	}
}
