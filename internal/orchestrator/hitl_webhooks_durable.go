package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/secrets"
)

// maxWebhookSends bounds the one-way webhook sends in flight, so a restate
// that accepts connections and never answers holds at most this many.
const maxWebhookSends = 8

// errWebhookSealed is a delivery whose endpoint no longer decrypts, as after
// TOKEN_ENCRYPTION_KEY changed under it. a retry cannot change it.
var errWebhookSealed = errors.New("webhook endpoint does not decrypt under TOKEN_ENCRYPTION_KEY")

// WebhookEndpointSealed reports whether a webhook post failed because its
// endpoint no longer decrypts.
func WebhookEndpointSealed(err error) bool { return errors.Is(err, errWebhookSealed) }

// WebhookDelivery is the input of the durable Webhooks.Deliver handler, one
// attempt at one notification to one endpoint. restate journals it, so it
// holds nothing secret: the notification is ids, event, gate, deadline and
// the intervention link, and the endpoint url and signing secret travel
// sealed under TOKEN_ENCRYPTION_KEY. a webhook url can carry a token in its
// path or query, so it is never journaled in the clear.
type WebhookDelivery struct {
	// ID names the delivery across its attempts: the dead letter's entity id.
	ID           string            `json:"id"`
	Notification hitl.Notification `json:"notification"`
	// Endpoint is the url's host, the metrics label.
	Endpoint string `json:"endpoint"`
	// Target is the url and secret as json, encrypted.
	Target []byte `json:"target"`
	// Attempt counts from 1. FirstAt is when the delivery was enqueued; the
	// retry window is measured from it.
	Attempt int       `json:"attempt"`
	FirstAt time.Time `json:"first_at"`
}

// WebhookAttempt is what one post got back. restate journals it, so Error
// never carries the url: a transport error is reduced to its cause.
type WebhookAttempt struct {
	Delivered bool `json:"delivered"`
	// Status is the receiver's http status, 0 when none answered.
	Status int       `json:"status"`
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at"`
}

// sealedTarget is the plaintext inside WebhookDelivery.Target.
type sealedTarget struct {
	URL    string `json:"url"`
	Secret []byte `json:"secret"`
}

// DurableWebhooks runs hitl webhook deliveries as the restate
// Webhooks.Deliver handler. *durable.Client satisfies it. SendWebhook only
// enqueues the first attempt.
type DurableWebhooks interface {
	SendWebhook(ctx context.Context, d WebhookDelivery) error
}

// WebhookMetrics is an optional extension of ReconcileMetrics, counting hitl
// webhook deliveries by endpoint host.
type WebhookMetrics interface {
	// WebhookAttempted is one post; status is 0 when no receiver answered.
	WebhookAttempted(endpoint string, status int, err error)
	// WebhookBacklog is how many durable deliveries to endpoint are not yet
	// delivered or dead-lettered.
	WebhookBacklog(endpoint string, n int)
	WebhookDeadLettered(endpoint string)
}

// SetDurableWebhooks routes hitl webhook deliveries through restate. nil
// (mode off, the default) keeps the in-process retry. without a
// TOKEN_ENCRYPTION_KEY deliveries stay in process too: the endpoint could
// not be sealed for the journal, and the signing secrets are memory only
// anyway. set it before Start.
func (fm *FleetManager) SetDurableWebhooks(d DurableWebhooks) {
	fm.mu.Lock()
	fm.durableHooks = d
	if fm.webhookSendSlots == nil {
		fm.webhookSendSlots = make(chan struct{}, maxWebhookSends)
	}
	fm.mu.Unlock()
}

func (fm *FleetManager) durableWebhooks() DurableWebhooks {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableHooks
}

// notifyWebhook delivers n to t: a one-way send to restate, or the
// in-process retry in mode off. the send runs off the caller's goroutine, at
// most maxWebhookSends at a time. a send restate does not take falls back to
// the in-process retry, so the notification is not dropped.
func (fm *FleetManager) notifyWebhook(ctx context.Context, t webhookTarget, n hitl.Notification) {
	hooks := fm.durableWebhooks()
	if hooks == nil || len(fm.tokenEncryptionKey) != 32 {
		go fm.webhooks.deliver(fm, t, n)
		return
	}
	d, err := fm.newWebhookDelivery(t, n)
	if err != nil {
		fm.logger.Warn("seal hitl webhook endpoint failed, delivering in process", "endpoint", webhookEndpoint(t.url), "err", err)
		go fm.webhooks.deliver(fm, t, n)
		return
	}
	fm.webhookPending(d, true)
	// the send keeps the request id but not the request's deadline.
	ctx = context.WithoutCancel(ctx)
	fm.webhookSends.Add(1)
	go func() {
		defer fm.webhookSends.Done()
		fm.webhookSendSlots <- struct{}{}
		defer func() { <-fm.webhookSendSlots }()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := hooks.SendWebhook(ctx, d); err != nil {
			fm.logger.Error("send durable hitl webhook failed, delivering in process", "endpoint", d.Endpoint, "event", n.Event, "intervention", n.InterventionID, "err", err)
			fm.webhookPending(d, false)
			fm.webhooks.deliver(fm, t, n)
		}
	}()
}

// newWebhookDelivery is the first attempt of n to t, with t sealed.
func (fm *FleetManager) newWebhookDelivery(t webhookTarget, n hitl.Notification) (WebhookDelivery, error) {
	raw, err := json.Marshal(sealedTarget{URL: t.url, Secret: t.secret})
	if err != nil {
		return WebhookDelivery{}, err
	}
	sealed, err := secrets.EncryptBytes(raw, fm.tokenEncryptionKey)
	if err != nil {
		return WebhookDelivery{}, err
	}
	id := NewEventID()
	if id == "" {
		return WebhookDelivery{}, errors.New("generate delivery id")
	}
	return WebhookDelivery{
		ID:           id,
		Notification: n,
		Endpoint:     webhookEndpoint(t.url),
		Target:       sealed,
		Attempt:      1,
		FirstAt:      time.Now().UTC(),
	}, nil
}

// the methods below are the steps of a durable delivery. the handler runs
// each as a journaled step.

// WebhookPost makes one attempt at d. a receiver that does not answer 2xx is
// a failed attempt, reported in the result, not an error: the handler
// decides whether to try again later. the only error is an endpoint that no
// longer decrypts.
func (fm *FleetManager) WebhookPost(_ context.Context, d WebhookDelivery) (WebhookAttempt, error) {
	raw, err := secrets.DecryptBytes(d.Target, fm.tokenEncryptionKey)
	if err != nil {
		return WebhookAttempt{}, errWebhookSealed
	}
	var st sealedTarget
	if err := json.Unmarshal(raw, &st); err != nil {
		return WebhookAttempt{}, errWebhookSealed
	}
	body, err := json.Marshal(d.Notification)
	if err != nil {
		return WebhookAttempt{}, err
	}
	client := fm.webhooks.client
	if client == nil {
		client = defaultWebhookClient
	}
	status, err := postWebhook(client, webhookTarget{url: st.URL, secret: st.Secret}, body)
	fm.webhookAttempted(d.Endpoint, status, err)
	out := WebhookAttempt{Delivered: err == nil, Status: status, At: time.Now().UTC()}
	if err != nil {
		out.Error = webhookError(err)
	}
	// a delivery that failed is still owed; after a restart this puts it
	// back in the backlog on its next attempt.
	fm.webhookPending(d, err != nil)
	return out, nil
}

// WebhookDeadLetter records a delivery that ran out of its retry window. a
// failed dead-letter write fails the step, so restate retries it.
func (fm *FleetManager) WebhookDeadLetter(ctx context.Context, d WebhookDelivery, lastErr string) error {
	if fm.store != nil {
		now := time.Now()
		if err := fm.store.UpsertDeadLetter(ctx, DeadLetterRecord{
			Kind:       DeadLetterWebhook,
			EntityID:   d.ID,
			Reason:     lastErr,
			RetryCount: d.Attempt,
			Payload: dlPayload(
				"event", string(d.Notification.Event),
				"intervention_id", d.Notification.InterventionID,
				"env_id", d.Notification.EnvID,
				"endpoint", d.Endpoint,
				"attempts", strconv.Itoa(d.Attempt),
			),
			FirstSeenAt: d.FirstAt,
			LastSeenAt:  now,
		}); err != nil {
			return fmt.Errorf("record webhook dead letter %s: %w", d.ID, err)
		}
	}
	fm.webhookPending(d, false)
	if m, ok := fm.metrics.(WebhookMetrics); ok {
		m.WebhookDeadLettered(d.Endpoint)
	}
	fm.logger.ErrorContext(ctx, "hitl webhook dead-lettered", "delivery", d.ID, "endpoint", d.Endpoint,
		"event", d.Notification.Event, "intervention", d.Notification.InterventionID, "attempts", d.Attempt, "err", lastErr)
	return nil
}

// webhookPending adds d to its endpoint's backlog, or takes it out, and
// reports the endpoint's new count. the backlog is this process's view: a
// delivery waiting out its backoff across a restart rejoins it on its next
// attempt.
func (fm *FleetManager) webhookPending(d WebhookDelivery, pending bool) {
	fm.webhookMu.Lock()
	if fm.webhookBacklog == nil {
		fm.webhookBacklog = map[string]map[string]struct{}{}
	}
	ids := fm.webhookBacklog[d.Endpoint]
	if pending {
		if ids == nil {
			ids = map[string]struct{}{}
			fm.webhookBacklog[d.Endpoint] = ids
		}
		ids[d.ID] = struct{}{}
	} else {
		delete(ids, d.ID)
	}
	n := len(ids)
	fm.webhookMu.Unlock()
	if m, ok := fm.metrics.(WebhookMetrics); ok {
		m.WebhookBacklog(d.Endpoint, n)
	}
}

// webhookAttempted reports one post, in either mode.
func (fm *FleetManager) webhookAttempted(endpoint string, status int, err error) {
	if m, ok := fm.metrics.(WebhookMetrics); ok {
		m.WebhookAttempted(endpoint, status, err)
	}
}

// webhookEndpoint is the host (and port) of a webhook url: the metrics and
// log label. the path and query can carry a token, so they are left out.
func webhookEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid"
	}
	return u.Host
}

// webhookError is err without the request url, which a transport error
// spells out in full, query string included.
func webhookError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err.Error()
	}
	return err.Error()
}
