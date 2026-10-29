package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// AuditFilter narrows ListHitlAudit. empty fields match everything; rows
// come back oldest first.
type AuditFilter struct {
	// EnvIDs limits rows to these environments. nil matches every env; an
	// empty non-nil list matches none.
	EnvIDs   []string
	ActorID  string
	Since    time.Time
	AfterSeq int64
	Limit    int
}

// HitlAuditListFilter is the api-facing filter, with an opaque cursor.
type HitlAuditListFilter struct {
	EnvIDs  []string
	ActorID string
	Since   time.Time
	Limit   int
	Cursor  string
}

type auditCursor struct {
	Seq int64 `json:"seq"`
}

// auditMeta is what a transition adds to its audit row and notification
// beyond the intervention itself.
type auditMeta struct {
	remoteAddr string
	// targets overrides the webhook lookup by env id, for a transition whose
	// environment is already gone from the fleet (cancel on destroy).
	targets []webhookTarget
}

// RecordHitlAudit appends one audit row and notifies the environment's
// webhooks. the api calls it for invite events; intervention transitions go
// through auditIntervention.
func (fm *FleetManager) RecordHitlAudit(ctx context.Context, e hitl.AuditEntry) {
	fm.recordAudit(ctx, e, fm.webhookTargetsFor(e.EnvID), nil)
}

// recordAudit appends e and sends its notification. an append failure is
// logged loudly but does not fail the action it records: the action has
// already happened, and refusing to report it would not undo it.
func (fm *FleetManager) recordAudit(ctx context.Context, e hitl.AuditEntry, targets []webhookTarget, in *hitl.Intervention) {
	e.At = hitl.AuditTime(time.Now())
	if _, err := fm.store.AppendHitlAudit(ctx, e); err != nil {
		fm.logger.Error("append hitl audit row failed", "event", e.Event, "intervention", e.InterventionID, "env", e.EnvID, "err", err)
	}
	if len(targets) == 0 {
		return
	}
	n := hitl.Notification{Event: e.Event, InterventionID: e.InterventionID, EnvID: e.EnvID}
	if in != nil {
		n.On = string(in.Kind)
		d := in.Deadline.UTC()
		n.Deadline = &d
	}
	fm.mu.RLock()
	base := fm.publicURL
	fm.mu.RUnlock()
	if base != "" && e.InterventionID != "" {
		n.URL = base + "/v1/interventions/" + url.PathEscape(e.InterventionID)
	}
	for _, t := range targets {
		go fm.webhooks.deliver(fm, t, n)
	}
}

// auditIntervention records an intervention transition.
func (fm *FleetManager) auditIntervention(ctx context.Context, in hitl.Intervention, meta auditMeta) {
	var (
		event hitl.AuditEvent
		actor hitl.Actor
	)
	switch in.State {
	case hitl.StatePending:
		event, actor = hitl.AuditRaised, in.TriggeredBy
	case hitl.StateApproved, hitl.StateRejected:
		event, actor = hitl.AuditDecided, in.Decision.DecidedBy
	case hitl.StateExpired:
		event, actor = hitl.AuditExpired, in.Decision.DecidedBy
	case hitl.StateCancelled:
		event, actor = hitl.AuditCancelled, in.Decision.DecidedBy
	default:
		return
	}
	targets := meta.targets
	if targets == nil {
		targets = fm.webhookTargetsFor(in.EnvID)
	}
	fm.recordAudit(ctx, hitl.AuditEntry{
		Event:          event,
		InterventionID: in.ID,
		EnvID:          in.EnvID,
		Actor:          actor,
		RemoteAddr:     meta.remoteAddr,
	}, targets, &in)
}

// ListHitlAudit returns one page of audit rows, oldest first, and a cursor
// for the next page (empty when there is none).
func (fm *FleetManager) ListHitlAudit(ctx context.Context, f HitlAuditListFilter) ([]hitl.AuditEntry, string, error) {
	limit := clampLimit(f.Limit)
	var after int64
	if f.Cursor != "" {
		var c auditCursor
		if err := decodeCursor(f.Cursor, &c); err != nil {
			return nil, "", err
		}
		after = c.Seq
	}
	rows, err := fm.store.ListHitlAudit(ctx, AuditFilter{
		EnvIDs: f.EnvIDs, ActorID: f.ActorID, Since: f.Since, AfterSeq: after, Limit: limit + 1,
	})
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(rows) > limit {
		rows = rows[:limit]
		next, _ = encodeCursor(auditCursor{Seq: rows[len(rows)-1].Seq})
	}
	return rows, next, nil
}

// webhookTarget is one resolved notify destination.
type webhookTarget struct {
	url    string
	secret []byte
}

// webhookTargetsFor returns the notify targets of a live environment.
func (fm *FleetManager) webhookTargetsFor(envID string) []webhookTarget {
	if envID == "" {
		return nil
	}
	fm.mu.RLock()
	v := fm.vms[envID]
	fm.mu.RUnlock()
	return fm.webhookTargets(v)
}

// webhookTargets resolves a vm's hitl.notify entries against its signing
// secrets. an entry whose secret is unavailable (sealed with no key after a
// restart) is skipped with a warning rather than sent unsigned.
func (fm *FleetManager) webhookTargets(v *vm) []webhookTarget {
	if v == nil || v.spec.Hitl == nil || len(v.spec.Hitl.Notify) == 0 {
		return nil
	}
	out := make([]webhookTarget, 0, len(v.spec.Hitl.Notify))
	for _, w := range v.spec.Hitl.Notify {
		secret, ok := v.hitlKeys[w.Secret]
		if !ok {
			fm.logger.Warn("hitl webhook skipped: signing secret unavailable", "vm", v.id, "secret", w.Secret)
			continue
		}
		out = append(out, webhookTarget{url: w.URL, secret: []byte(secret)})
	}
	return out
}

// webhookSender delivers notifications with retry. the zero value uses the
// defaults; tests shorten the backoff.
type webhookSender struct {
	client  *http.Client
	backoff []time.Duration
}

var defaultWebhookBackoff = []time.Duration{0, time.Second, 4 * time.Second, 16 * time.Second}

var defaultWebhookClient = &http.Client{
	Timeout: 10 * time.Second,
	// a redirect would send the signed body somewhere the author did not
	// name, so it counts as a failure.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// deliver posts n to t, retrying on any non-2xx answer or transport error.
// deliveries are best-effort and not persisted: the audit row is the
// record, the webhook is a nudge.
func (s webhookSender) deliver(fm *FleetManager, t webhookTarget, n hitl.Notification) {
	client, backoff := s.client, s.backoff
	if client == nil {
		client = defaultWebhookClient
	}
	if backoff == nil {
		backoff = defaultWebhookBackoff
	}
	body, err := json.Marshal(n)
	if err != nil {
		fm.logger.Warn("encode hitl webhook failed", "err", err)
		return
	}
	var lastErr error
	for _, wait := range backoff {
		time.Sleep(wait)
		if lastErr = postWebhook(client, t, body); lastErr == nil {
			return
		}
	}
	fm.logger.Warn("hitl webhook delivery failed", "url", t.url, "event", n.Event, "intervention", n.InterventionID, "err", lastErr)
}

func postWebhook(client *http.Client, t webhookTarget, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fuse-orchestrator")
	req.Header.Set(hitl.HeaderSignature, hitl.Sign(t.secret, time.Now(), body))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("receiver answered %d", resp.StatusCode)
	}
	return nil
}
