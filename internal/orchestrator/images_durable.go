package orchestrator

import (
	"context"
	"fmt"
	"time"
)

// ImageFleetErrors are the sentinel errors a convert fails with. the durable
// handler carries each one across restate as its own error code, so the api
// answers it with the same status as in mode off.
var ImageFleetErrors = []error{
	ErrInvalidImage,
	ErrImageNotFound,
	ErrImageUnsupported,
	ErrImageHostFailed,
	ErrNoHosts,
	ErrHostNotFound,
	ErrNoCapacity,
	ErrImageCacheFull,
}

// ImageWatchTimeout bounds how long a durable convert polls its host. it is
// longer than the agent's own bake timeout (FC_IMAGE_CONVERT_TIMEOUT, 1800s
// by default), so the agent normally ends a stuck bake first.
const ImageWatchTimeout = time.Hour

// DurableImages runs a convert as the restate Images.Convert handler and
// waits for its first step, the agent call. the handler then watches the
// conversion on its own. *durable.Client satisfies it.
type DurableImages interface {
	ConvertImage(ctx context.Context, req ConvertImageRequest) (Image, error)
}

// SetDurableImages routes image converts through restate. nil (mode off, the
// default) keeps the in-process convert. set it before Start.
func (fm *FleetManager) SetDurableImages(d DurableImages) {
	fm.mu.Lock()
	fm.durableImage = d
	fm.mu.Unlock()
}

// durableImages is the converter converts go through, or nil for in process.
func (fm *FleetManager) durableImages() DurableImages {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableImage
}

// ImageHost is the first step a durable convert runs: the host req converts
// on, picked the way ImageConvert picks it. the convert step then runs pinned
// to it, so a retry after a restart reaches the same host.
func (fm *FleetManager) ImageHost(_ context.Context, req ConvertImageRequest) (string, error) {
	hostID, _, err := fm.imageConvertHost(req)
	return hostID, err
}

// the methods below are the steps a durable convert runs after ImageConvert:
// a poll of the host, repeated, and the outcome. each runs inside a journaled
// restate step and must be safe to run again.

// ImagePoll reads img's record on the host it converts on and returns img
// with the host's state and error. a host that cannot be asked counts as
// still converting, so the watch keeps polling until its bound. a host that
// is gone, or no longer has the record, ends the watch as failed. a record
// from a later attempt means img's attempt failed and was retried between
// polls: that ends the watch as failed too, and the retry's own watch
// reports the new attempt.
func (fm *FleetManager) ImagePoll(ctx context.Context, img Image) (Image, error) {
	fm.mu.RLock()
	p, ok := fm.providerForHost(img.HostID)
	fm.mu.RUnlock()
	conv, isConv := p.(ImageConverter)
	if !ok || !isConv {
		img.State = ImageFailed
		img.Error = fmt.Sprintf("host %s is no longer registered", img.HostID)
		return img, nil
	}
	cctx, cancel := context.WithTimeout(ctx, imageGetTimeout)
	defer cancel()
	got, err := conv.GetImage(cctx, img.Digest)
	switch {
	case IsNotFound(err):
		img.State = ImageFailed
		img.Error = fmt.Sprintf("host %s no longer has a record of the image", img.HostID)
	case err != nil:
		fm.logger.WarnContext(ctx, "image status failed", "digest", img.Digest, "host", img.HostID, "err", err)
	case img.CreatedAt != "" && got.CreatedAt != "" && got.CreatedAt != img.CreatedAt:
		img.State = ImageFailed
		img.Error = fmt.Sprintf("the attempt failed and was retried (new attempt started %s)", got.CreatedAt)
	default:
		img.State = got.State
		img.Error = got.Error
	}
	return img, nil
}

// ImageConvertDone records how a durable convert ended: logged, and an
// image.ready or image.failed event. timedOut is a watch that hit
// ImageWatchTimeout with the host still converting; the host keeps the
// record and ends the bake itself.
func (fm *FleetManager) ImageConvertDone(ctx context.Context, img Image, timedOut bool) error {
	attrs := []any{"digest", img.Digest, "ref", img.Ref, "host", img.HostID}
	event := "image." + string(img.State)
	switch {
	case timedOut:
		event = "image.watch_timed_out"
		fm.logger.WarnContext(ctx, "image convert still running past the watch bound", append(attrs, "bound", ImageWatchTimeout)...)
	case img.State == ImageReady:
		fm.logger.InfoContext(ctx, "image converted", attrs...)
	default:
		fm.logger.ErrorContext(ctx, "image convert failed", append(attrs, "err", img.Error)...)
	}
	// the error goes back so the durable step retries instead of journaling
	// a done whose event never landed.
	return fm.writeEvent(ctx, "image", img.Digest, event, map[string]any{
		"ref":     img.Ref,
		"host_id": img.HostID,
		"error":   img.Error,
	})
}
