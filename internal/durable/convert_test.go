package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// imageFleet scripts a convert: the agent call answers with one record, and
// each poll answers with the next state in polls (the last one repeats). a
// poll can block until released. done gets the record the watch ended with.
type imageFleet struct {
	fakeFleet

	mu           sync.Mutex
	calls        []string
	host         string
	hostReqs     []orchestrator.ConvertImageRequest
	convertReqs  []orchestrator.ConvertImageRequest
	convertBlock chan struct{}
	img          orchestrator.Image
	convertErr   error
	polls        []orchestrator.ImageState
	block        chan struct{}
	entered      chan struct{}
	done         chan imageDone
}

type imageDone struct {
	img      orchestrator.Image
	timedOut bool
}

func newImageFleet(name string, polls ...orchestrator.ImageState) *imageFleet {
	now := time.Now().UnixNano()
	return &imageFleet{
		img: orchestrator.Image{
			Digest: fmt.Sprintf("%064d", now), Ref: name, HostID: "fc-1",
			State: orchestrator.ImageConverting, CreatedAt: fmt.Sprint(now),
		},
		host:    "fc-1",
		polls:   polls,
		entered: make(chan struct{}, 16),
		done:    make(chan imageDone, 4),
	}
}

func (f *imageFleet) record(step string) {
	f.mu.Lock()
	f.calls = append(f.calls, step)
	f.mu.Unlock()
}

func (f *imageFleet) steps() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func (f *imageFleet) ImageHost(_ context.Context, req orchestrator.ConvertImageRequest) (string, error) {
	f.record("place")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostReqs = append(f.hostReqs, req)
	return f.host, nil
}

func (f *imageFleet) ImageConvert(ctx context.Context, req orchestrator.ConvertImageRequest) (orchestrator.Image, error) {
	f.record("convert")
	f.mu.Lock()
	f.convertReqs = append(f.convertReqs, req)
	block := f.convertBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		select {
		case <-block:
		case <-ctx.Done():
			return orchestrator.Image{}, ctx.Err()
		}
	}
	return f.img, f.convertErr
}

func (f *imageFleet) ImagePoll(ctx context.Context, img orchestrator.Image) (orchestrator.Image, error) {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		select {
		case <-block:
		case <-ctx.Done():
			return img, ctx.Err()
		}
	}
	// recorded once the poll is let through, so a poll that starts while a
	// convert is still answering lands after it in the steps.
	f.record("poll")
	f.mu.Lock()
	defer f.mu.Unlock()
	img.State = f.polls[0]
	if len(f.polls) > 1 {
		f.polls = f.polls[1:]
	}
	if img.State == orchestrator.ImageFailed {
		img.Error = "fc-bake-distro-rootfs: unsupported distro"
	}
	return img, nil
}

func (f *imageFleet) ImageConvertDone(_ context.Context, img orchestrator.Image, timedOut bool) error {
	f.record("done")
	f.done <- imageDone{img: img, timedOut: timedOut}
	return nil
}

// fastImagePolls shortens the watch's sleeps for the test.
func fastImagePolls(t *testing.T) {
	first, max, bound := imagePollFirst, imagePollMax, imageWatchBound
	imagePollFirst, imagePollMax = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { imagePollFirst, imagePollMax, imageWatchBound = first, max, bound })
}

func waitImageDone(t *testing.T, f *imageFleet) imageDone {
	t.Helper()
	select {
	case d := <-f.done:
		return d
	case <-time.After(30 * time.Second):
		t.Fatalf("watch did not finish; steps %s", f.steps())
		return imageDone{}
	}
}

// TestConvertImageThroughRestate: a convert answers with the agent's record
// at once, a duplicate convert of the same conversion joins the first's
// watch, and the watch polls until the image is ready.
func TestConvertImageThroughRestate(t *testing.T) {
	fastImagePolls(t)
	fleet := newImageFleet("ubuntu:24.04", orchestrator.ImageConverting, orchestrator.ImageConverting, orchestrator.ImageReady)
	// the first poll waits for both converts, so the steps come in order.
	fleet.block = make(chan struct{})
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := orchestrator.ConvertImageRequest{Ref: "ubuntu:24.04"}
	for i := 0; i < 2; i++ {
		img, err := client.ConvertImage(ctx, req)
		if err != nil {
			t.Fatalf("convert %d: %v", i, err)
		}
		if img.Digest != fleet.img.Digest || img.State != orchestrator.ImageConverting || img.HostID != "fc-1" {
			t.Fatalf("convert %d answered %+v", i, img)
		}
	}
	close(fleet.block)
	d := waitImageDone(t, fleet)
	if d.timedOut || d.img.State != orchestrator.ImageReady || d.img.Ref != "ubuntu:24.04" {
		t.Fatalf("watch ended with %+v", d)
	}
	// one watch only: a second one would poll again and end a second time.
	select {
	case extra := <-fleet.done:
		t.Fatalf("a second watch ran: %+v; steps %s", extra, fleet.steps())
	case <-time.After(500 * time.Millisecond):
	}
	if got, want := fleet.steps(), "place,convert,place,convert,poll,poll,poll,done"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}

// TestConvertImageFailedWatch: a bake that fails ends the watch failed, with
// the host's reason.
func TestConvertImageFailedWatch(t *testing.T) {
	fastImagePolls(t)
	fleet := newImageFleet("busybox", orchestrator.ImageFailed)
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.ConvertImage(ctx, orchestrator.ConvertImageRequest{Ref: "busybox"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	d := waitImageDone(t, fleet)
	if d.timedOut || d.img.State != orchestrator.ImageFailed || !strings.Contains(d.img.Error, "unsupported distro") {
		t.Fatalf("watch ended with %+v", d)
	}
}

// TestConvertImageRetryWithoutAttemptGetsAWatch: an agent that predates
// created_at answers a retry of a failed image with the same host and digest;
// the retry still gets a watch of its own instead of the first one's.
func TestConvertImageRetryWithoutAttemptGetsAWatch(t *testing.T) {
	fastImagePolls(t)
	fleet := newImageFleet("busybox", orchestrator.ImageFailed)
	fleet.img.CreatedAt = ""
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if _, err := client.ConvertImage(ctx, orchestrator.ConvertImageRequest{Ref: "busybox"}); err != nil {
			t.Fatalf("convert %d: %v", i, err)
		}
		if d := waitImageDone(t, fleet); d.img.State != orchestrator.ImageFailed {
			t.Fatalf("watch %d ended with %+v", i, d)
		}
	}
}

// TestConvertImageReadyStartsNoWatch: an image the host already has ready
// answers at once and starts no watch.
func TestConvertImageReadyStartsNoWatch(t *testing.T) {
	fastImagePolls(t)
	fleet := newImageFleet("ubuntu", orchestrator.ImageReady)
	fleet.img.State = orchestrator.ImageReady
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	img, err := client.ConvertImage(ctx, orchestrator.ConvertImageRequest{Ref: "ubuntu"})
	if err != nil || img.State != orchestrator.ImageReady {
		t.Fatalf("convert: %+v, %v", img, err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := fleet.steps(); got != "place,convert" {
		t.Fatalf("steps %s, want place,convert", got)
	}
}

// TestConvertImageRefusalKeepsSentinel: a refusal crosses restate as its
// sentinel, so the api answers it with that sentinel's status, and is not
// retried.
func TestConvertImageRefusalKeepsSentinel(t *testing.T) {
	for _, sentinel := range []error{orchestrator.ErrInvalidImage, orchestrator.ErrImageHostFailed, orchestrator.ErrNoCapacity} {
		fleet := newImageFleet("ubuntu")
		fleet.convertErr = fmt.Errorf("%w: refused", sentinel)
		client, _, _ := serveBuildFleet(t, fleet, "")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := client.ConvertImage(ctx, orchestrator.ConvertImageRequest{Ref: "ubuntu"})
		cancel()
		if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("convert error %v, want %v", err, sentinel)
		}
		if got := fleet.steps(); got != "place,convert" {
			t.Fatalf("steps %s, want place,convert", got)
		}
	}
}

// TestConvertImageWatchTimesOut: a host that never finishes ends the watch at
// its bound, recorded as timed out rather than failed.
func TestConvertImageWatchTimesOut(t *testing.T) {
	fastImagePolls(t)
	imageWatchBound = 200 * time.Millisecond
	fleet := newImageFleet("ubuntu", orchestrator.ImageConverting)
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.ConvertImage(ctx, orchestrator.ConvertImageRequest{Ref: "ubuntu"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	d := waitImageDone(t, fleet)
	if !d.timedOut || d.img.State != orchestrator.ImageConverting {
		t.Fatalf("watch ended with %+v", d)
	}
}

// TestConvertImageWatchSurvivesEndpointRestart stands in for kill -9 of the
// orchestrator mid-bake: the endpoint drops while the watch polls, and the
// restarted one resumes polling without converting again.
func TestConvertImageWatchSurvivesEndpointRestart(t *testing.T) {
	fastImagePolls(t)
	first := newImageFleet("ubuntu", orchestrator.ImageConverting)
	first.block = make(chan struct{})
	defer close(first.block)
	client, addr, stop := serveBuildFleet(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.ConvertImage(ctx, orchestrator.ConvertImageRequest{Ref: "ubuntu"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	select {
	case <-first.entered:
	case <-time.After(20 * time.Second):
		t.Fatalf("poll never started; steps %s", first.steps())
	}
	stop()

	second := newImageFleet("ubuntu", orchestrator.ImageReady)
	_, _, _ = serveBuildFleet(t, second, addr)
	if d := waitImageDone(t, second); d.img.State != orchestrator.ImageReady || d.img.Digest != first.img.Digest {
		t.Fatalf("watch ended with %+v", d)
	}
	if got, want := second.steps(), "poll,done"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
}

// TestConvertImageKeepsHostAcrossRestart stands in for kill -9 of the
// orchestrator during the agent call, with the fleet's hosts changed by the
// time it comes back: the retried agent call goes to the host the first try
// picked, not to a fresh pick.
func TestConvertImageKeepsHostAcrossRestart(t *testing.T) {
	fastImagePolls(t)
	first := newImageFleet("ubuntu", orchestrator.ImageReady)
	first.convertBlock = make(chan struct{})
	defer close(first.convertBlock)
	client, addr, stop := serveBuildFleet(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type result struct {
		img orchestrator.Image
		err error
	}
	res := make(chan result, 1)
	go func() {
		img, err := client.ConvertImage(ctx, orchestrator.ConvertImageRequest{Ref: "ubuntu"})
		res <- result{img, err}
	}()
	select {
	case <-first.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("agent call never started; steps %s", first.steps())
	}
	stop()

	// the restarted orchestrator would now pick another host.
	second := newImageFleet("ubuntu", orchestrator.ImageReady)
	second.img = first.img
	second.host = "fc-2"
	_, _, _ = serveBuildFleet(t, second, addr)
	r := <-res
	if r.err != nil {
		t.Fatalf("convert: %v", r.err)
	}
	if got, want := second.steps(), "convert"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
	if len(second.convertReqs) != 1 || second.convertReqs[0].HostID != "fc-1" {
		t.Fatalf("retried agent call went to %+v, want host fc-1", second.convertReqs)
	}
}
