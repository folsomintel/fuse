package durable

import (
	"context"
	"errors"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opImage is the op label on an image convert's step spans and metrics.
const opImage = "image_convert"

// imageFailedCode is the error code a convert failure without a sentinel
// crosses restate as, so the client hands the api the fleet's message
// unchanged. imageErrorBase+i carries orchestrator.ImageFleetErrors[i]. clear
// of the 480s, create's 530s, migrate's 540s and fork's 550s.
const (
	imageFailedCode restate.Code = 560
	imageErrorBase  restate.Code = 561
)

// the watch's poll interval, doubling from first to max, and its overall
// bound. vars so tests can shorten them.
var (
	imagePollFirst  = 5 * time.Second
	imagePollMax    = 30 * time.Second
	imageWatchBound = orchestrator.ImageWatchTimeout
)

// Images is the part of the fleet the convert and watch handlers drive, one
// method per step. *FleetManager satisfies it.
type Images interface {
	ImageHost(ctx context.Context, req orchestrator.ConvertImageRequest) (string, error)
	ImageConvert(ctx context.Context, req orchestrator.ConvertImageRequest) (orchestrator.Image, error)
	ImagePoll(ctx context.Context, img orchestrator.Image) (orchestrator.Image, error)
	ImageConvertDone(ctx context.Context, img orchestrator.Image, timedOut bool) error
}

// convertImage is Images.Convert: a step that picks the host, then the agent
// call that resolves the ref and starts the bake there, as a second step, then
// a send of Images.Watch for a conversion still running. the host is its own
// step so a restart mid agent call sends the retry to the host the first try
// reached, even if the fleet's hosts changed in between. the ref only becomes a digest on the host, so the watch is
// what is keyed by it: its idempotency key is the host, the digest and the
// attempt's start, so a duplicate or retried convert of the same ref on the
// same host joins the watch the first one started, and a retry of a failed
// image (a new attempt) gets a watch of its own. the agent call is keyed on
// the step, so a restart before it was journaled gets the agent's first
// answer back instead of starting another bake.
func convertImage(ctx restate.Context, fleet Images, req orchestrator.ConvertImageRequest) (orchestrator.Image, error) {
	hostID, err := Run(ctx, opImage, "place", func(c context.Context) (string, error) {
		hostID, err := fleet.ImageHost(c, req)
		return hostID, imageTerminal(err)
	})
	if err != nil {
		return orchestrator.Image{}, err
	}
	req.HostID = hostID
	img, err := Run(ctx, opImage, "convert", func(c context.Context) (orchestrator.Image, error) {
		img, err := fleet.ImageConvert(c, req)
		return img, imageTerminal(err)
	})
	if err != nil {
		return orchestrator.Image{}, err
	}
	if img.State == orchestrator.ImageConverting {
		restate.ServiceSend(ctx, ImagesService, "Watch").Send(img, imageWatchKey(img)...)
	}
	return img, nil
}

// imageWatchKey is the idempotency key of img's watch. an agent that predates
// created_at gives no attempt to key on, and a key of host and digest alone
// would let a retry of a failed image join the first attempt's finished watch,
// so such a convert gets a watch of its own, unkeyed.
func imageWatchKey(img orchestrator.Image) []restate.SendOption {
	if img.CreatedAt == "" {
		return nil
	}
	return []restate.SendOption{restate.WithIdempotencyKey("image-watch:" + img.HostID + ":" + img.Digest + ":" + img.CreatedAt)}
}

// imageTerminal marks every convert failure terminal: each one is the api's
// answer at once, not something a retry fixes, under the sentinel's code.
func imageTerminal(err error) error {
	if err == nil {
		return nil
	}
	if i := sentinelIndex(err, orchestrator.ImageFleetErrors); i >= 0 {
		return restate.ToTerminalError(err, restate.WithErrorCode(imageErrorBase+restate.Code(i)))
	}
	return restate.ToTerminalError(err, restate.WithErrorCode(imageFailedCode))
}

// watchImage is Images.Watch: poll the host with durable sleeps until the
// conversion is ready or failed, or imageWatchBound passes, then record the
// outcome. a restart replays the journal and resumes polling; nothing is
// converted again. the agent removes a failed bake's partial rootfs itself,
// so a failure has nothing to undo here.
func watchImage(ctx restate.Context, fleet Images, policy RetryPolicy, img orchestrator.Image) error {
	start, err := nowMillis(ctx)
	if err != nil {
		return err
	}
	deadline := start + imageWatchBound.Milliseconds()
	wait := imagePollFirst
	for {
		if err := restate.Sleep(ctx, wait); err != nil {
			return err
		}
		polled, err := Run(ctx, opImage, "poll", func(c context.Context) (orchestrator.Image, error) {
			return fleet.ImagePoll(c, img)
		}, retryUnder(policy)...)
		if err != nil {
			return err
		}
		now, err := nowMillis(ctx)
		if err != nil {
			return err
		}
		finished := polled.State != orchestrator.ImageConverting
		if finished || now >= deadline {
			return runStep(ctx, opImage, "done", func(c context.Context) error {
				return fleet.ImageConvertDone(c, polled, !finished)
			}, retryUnder(policy)...)
		}
		wait = min(wait*2, imagePollMax)
	}
}

// ConvertImage runs Images.Convert and waits for it: the agent call, which
// resolves the ref against its registry, so it goes through the client with
// no request timeout of its own; ctx bounds the wait.
func (c *Client) ConvertImage(ctx context.Context, req orchestrator.ConvertImageRequest) (orchestrator.Image, error) {
	img, err := ingress.Service[orchestrator.ConvertImageRequest, orchestrator.Image](c.longIngress, ImagesService, "Convert").
		Request(ctx, req, callerKey(ctx)...)
	return img, imageError(err)
}

// imageError turns a convert's failure back into the fleet sentinel its code
// names, or into its own message, so the api answers it with that sentinel's
// status.
func imageError(err error) error {
	var ierr ingress.Error
	if !errors.As(err, &ierr) {
		return err
	}
	if ierr.Code() == imageFailedCode {
		return errors.New(ierr.Message())
	}
	i := int(ierr.Code()) - int(imageErrorBase)
	if i < 0 || i >= len(orchestrator.ImageFleetErrors) {
		return err
	}
	return &sentinelError{msg: ierr.Message(), sentinel: orchestrator.ImageFleetErrors[i]}
}

var _ orchestrator.DurableImages = (*Client)(nil)
