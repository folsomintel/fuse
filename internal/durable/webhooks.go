package durable

import (
	"context"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opWebhook is the op label on webhook delivery's step spans and metrics.
const opWebhook = "hitl_webhook"

// Webhooks is the part of the fleet the webhook delivery handler drives, one
// method per step. *FleetManager satisfies it.
type Webhooks interface {
	WebhookPost(ctx context.Context, d orchestrator.WebhookDelivery) (orchestrator.WebhookAttempt, error)
	WebhookDeadLetter(ctx context.Context, d orchestrator.WebhookDelivery, lastErr string) error
}

// the retry schedule of a delivery: a failed attempt waits
// webhookFirstBackoff, four times longer after each further failure, capped
// at webhookMaxBackoff, until the next attempt would land past webhookWindow
// from the first. vars so tests can shorten them.
var (
	webhookFirstBackoff = time.Second
	webhookMaxBackoff   = time.Hour
	webhookWindow       = 24 * time.Hour
)

// webhookBackoff is the wait after failed attempt n (counting from 1).
func webhookBackoff(attempt int) time.Duration {
	d := webhookFirstBackoff
	for i := 1; i < attempt && d < webhookMaxBackoff; i++ {
		d *= 4
	}
	return min(d, webhookMaxBackoff)
}

// deliverWebhook is Webhooks.Deliver: one attempt at one delivery. a failed
// attempt does not sleep in the invocation; it sends the next attempt to
// itself with a delay and ends, so a delivery that keeps failing for a day is
// a chain of short invocations, each on the deployment current when it
// starts. once the next attempt would fall outside the window, or the
// endpoint no longer decrypts, the delivery is dead-lettered and ends.
func deliverWebhook(ctx restate.Context, fleet Webhooks, d orchestrator.WebhookDelivery) error {
	res, err := Run(ctx, opWebhook, "post", func(c context.Context) (orchestrator.WebhookAttempt, error) {
		res, err := fleet.WebhookPost(c, d)
		if orchestrator.WebhookEndpointSealed(err) {
			return res, restate.ToTerminalError(err)
		}
		return res, err
	})
	lastErr := res.Error
	switch {
	case err != nil && !restate.IsTerminalError(err):
		// suspension or cancellation: let restate handle it.
		return err
	case err != nil:
		lastErr = err.Error()
	case res.Delivered:
		return nil
	default:
		wait := webhookBackoff(d.Attempt)
		if res.At.Add(wait).Sub(d.FirstAt) <= webhookWindow {
			next := d
			next.Attempt++
			restate.ServiceSend(ctx, WebhooksService, "Deliver").Send(next, restate.WithDelay(wait))
			return nil
		}
	}
	// the failure is recorded in the dead letter; the invocation itself is
	// done.
	return runStep(ctx, opWebhook, "dead_letter", func(c context.Context) error {
		return fleet.WebhookDeadLetter(c, d, lastErr)
	})
}

// SendWebhook enqueues the first attempt of d without waiting. it is keyed
// on d.ID, so sending d again starts nothing new.
func (c *Client) SendWebhook(ctx context.Context, d orchestrator.WebhookDelivery) error {
	_, err := ingress.ServiceSend[orchestrator.WebhookDelivery](c.ingress, WebhooksService, "Deliver").
		Send(ctx, d, restate.WithIdempotencyKey("webhook:"+d.ID))
	return err
}

var _ orchestrator.DurableWebhooks = (*Client)(nil)
