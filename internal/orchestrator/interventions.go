package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/secrets"
)

var (
	// ErrInterventionNotFound is returned for an unknown intervention id.
	ErrInterventionNotFound = errors.New("intervention not found")

	// ErrInterventionResolved is returned when deciding an intervention
	// that already reached a terminal state.
	ErrInterventionResolved = errors.New("intervention already resolved")

	// ErrHitlNotDeclared is returned when an environment raises a request
	// its hitl policy does not declare, including an environment created
	// with no hitl block at all.
	ErrHitlNotDeclared = errors.New("request kind not declared")

	// ErrInvalidIntervention is a malformed request or decision (empty
	// question, an answer outside the choices, a timeout out of bounds).
	ErrInvalidIntervention = errors.New("invalid intervention")
)

// HumanRequest is what an agent sends to raise a request.
type HumanRequest struct {
	Kind hitl.Kind
	Ask  hitl.AskPayload
	// Secret names the declared target and carries the agent's text.
	Secret hitl.SecretPayload
	// Takeover carries the way in and the agent's reason.
	Takeover hitl.TakeoverPayload
	// Review is the file path or inline content up for review.
	Review  hitl.ReviewPayload
	Timeout time.Duration // optional; can only shorten the policy's timeout
	By      hitl.Actor
	// RemoteAddr is the caller's address, recorded on the audit row.
	RemoteAddr string
}

// DecideRequest is a human's decision on an intervention.
type DecideRequest struct {
	Outcome hitl.Outcome
	// Answer is the reply to an ask; required to approve one.
	Answer string
	// Reason is fed back to the agent.
	Reason string
	// EditedArgv replaces a held exec's argv. it is re-checked against the
	// deny gates before the approval is accepted.
	EditedArgv []string
	// EditedText replaces a held type action's text, re-checked the same
	// way.
	EditedText *string
	// Grant is once, env or ttl:<duration>; empty takes the gate's default.
	Grant string
	// Extend is how long an approved expiry keeps the env alive; zero is
	// one more full window of the reaper that was due.
	Extend     time.Duration
	By         hitl.Actor
	RemoteAddr string
}

// InterventionListFilter narrows ListInterventions.
type InterventionListFilter struct {
	EnvID string
	// EnvIDs limits the list to these environments, on top of EnvID. nil
	// matches every env; a scoped key passes the envs it covers.
	EnvIDs []string
	// ActorID matches interventions raised or decided by this actor.
	ActorID string
	State   hitl.State
	Limit   int
	Cursor  string
}

type interventionCursor struct {
	ID string `json:"id"`
}

// RequestHuman opens a pending intervention for an environment. it returns
// at once; the caller waits with WaitIntervention.
func (fm *FleetManager) RequestHuman(ctx context.Context, envID string, req HumanRequest) (hitl.Intervention, error) {
	fm.mu.RLock()
	v, ok := fm.vms[envID]
	var (
		state VMState
		spec  *hitl.Spec
	)
	if ok {
		state = v.state
		spec = v.spec.Hitl
	}
	fm.mu.RUnlock()
	if !ok {
		return hitl.Intervention{}, fmt.Errorf("%w: %s", ErrVMNotFound, envID)
	}
	if state.tearingDown() {
		return hitl.Intervention{}, fmt.Errorf("%w: environment %s is being destroyed", ErrVMNotRunning, envID)
	}
	if spec == nil {
		return hitl.Intervention{}, fmt.Errorf("%w: environment %s was created without a hitl block", ErrHitlNotDeclared, envID)
	}
	if !spec.Allows(req.Kind) {
		return hitl.Intervention{}, fmt.Errorf("%w: environment %s does not declare hitl.requests.%s", ErrHitlNotDeclared, envID, req.Kind)
	}
	in := hitl.Intervention{Kind: req.Kind}
	switch req.Kind {
	case hitl.KindAsk:
		if err := req.Ask.Validate(); err != nil {
			return hitl.Intervention{}, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
		}
		in.Payload = req.Ask
	case hitl.KindSecret:
		if len(fm.tokenEncryptionKey) != 32 {
			return hitl.Intervention{}, fmt.Errorf("%w: refusing a secret request", ErrHitlNeedsKey)
		}
		t, ok := spec.Requests.Secret.Target(req.Secret.Target)
		if !ok {
			return hitl.Intervention{}, fmt.Errorf("%w: secret target %q is not declared under hitl.requests.secret.targets", ErrHitlNotDeclared, req.Secret.Target)
		}
		if err := hitl.ValidateSecretMessage(req.Secret.Message); err != nil {
			return hitl.Intervention{}, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
		}
		in.Secret = &hitl.SecretPayload{Target: t.Name(), Label: t.Label, Message: req.Secret.Message}
	case hitl.KindTakeover:
		via := req.Takeover.Via
		if via == "" {
			via = hitl.ViaTerminal
		}
		if !spec.Requests.Takeover.Allows(via) {
			return hitl.Intervention{}, fmt.Errorf("%w: environment %s does not allow takeover via %s", ErrHitlNotDeclared, envID, via)
		}
		if err := hitl.ValidateTakeoverReason(req.Takeover.Reason); err != nil {
			return hitl.Intervention{}, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
		}
		in.Takeover = &hitl.TakeoverPayload{Via: via, Reason: req.Takeover.Reason}
	case hitl.KindReview:
		p, err := hitl.ValidateReviewRequest(req.Review)
		if err != nil {
			return hitl.Intervention{}, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
		}
		if p.Path != "" {
			if p.Content, p.Truncated, err = fm.readReviewFile(ctx, envID, p.Path); err != nil {
				return hitl.Intervention{}, err
			}
		}
		in.Review = &p
	default:
		return hitl.Intervention{}, fmt.Errorf("%w: unknown request kind %q", ErrInvalidIntervention, req.Kind)
	}
	if req.Timeout < 0 {
		return hitl.Intervention{}, fmt.Errorf("%w: timeout must not be negative", ErrInvalidIntervention)
	}
	if req.Timeout > 0 {
		if err := hitl.ValidateTimeout(req.Timeout); err != nil {
			return hitl.Intervention{}, fmt.Errorf("%w: timeout %w", ErrInvalidIntervention, err)
		}
	}

	now := time.Now()
	in.ID = hitl.NewID(now)
	in.EnvID = envID
	in.State = hitl.StatePending
	in.TriggeredBy = req.By
	in.Deadline = hitl.Deadline(now, *spec, req.Timeout)
	in.OnTimeout = spec.EffectiveOnTimeout()
	in.CreatedAt, in.UpdatedAt = now, now
	if err := fm.raiseIntervention(ctx, in, req.RemoteAddr); err != nil {
		return hitl.Intervention{}, err
	}
	return in, nil
}

// raiseIntervention stores a new pending intervention and tells everyone
// who listens: the event log, the sse stream, the audit log and webhooks.
func (fm *FleetManager) raiseIntervention(ctx context.Context, in hitl.Intervention, remoteAddr string) error {
	envID := in.EnvID
	rec, err := fm.interventionToRecord(in)
	if err != nil {
		return err
	}
	if err := fm.store.UpsertIntervention(ctx, rec); err != nil {
		return err
	}
	fm.touchActivity(envID)
	fm.appendEvent(ctx, "intervention", in.ID, "intervention.raised", map[string]any{
		"env_id": envID, "kind": string(in.Kind), "deadline": in.Deadline,
	})
	fm.publishInterventionEvent(in)
	fm.auditIntervention(ctx, in, auditMeta{remoteAddr: remoteAddr})
	return nil
}

// GetIntervention returns one intervention. a pending one past its
// deadline is expired on the spot, so a reader never sees a stale pending.
func (fm *FleetManager) GetIntervention(ctx context.Context, id string) (hitl.Intervention, error) {
	rec, err := fm.store.GetIntervention(ctx, id)
	if err != nil {
		return hitl.Intervention{}, err
	}
	in, err := fm.interventionFromRecord(rec)
	if err != nil {
		return hitl.Intervention{}, err
	}
	if in.State == hitl.StatePending && hitl.Expired(time.Now(), in.Deadline) {
		return fm.expireIntervention(ctx, id)
	}
	return in, nil
}

// ListInterventions returns one page of interventions, oldest first, and a
// cursor for the next page (empty when there is none).
func (fm *FleetManager) ListInterventions(ctx context.Context, f InterventionListFilter) ([]hitl.Intervention, string, error) {
	limit := clampLimit(f.Limit)
	var after string
	if f.Cursor != "" {
		var c interventionCursor
		if err := decodeCursor(f.Cursor, &c); err != nil {
			return nil, "", err
		}
		after = c.ID
	}
	// one extra row tells us whether there is a next page.
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{
		EnvID: f.EnvID, EnvIDs: f.EnvIDs, ActorID: f.ActorID, State: string(f.State), After: after, Limit: limit + 1,
	})
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(recs) > limit {
		recs = recs[:limit]
		next, _ = encodeCursor(interventionCursor{ID: recs[len(recs)-1].ID})
	}
	out := make([]hitl.Intervention, 0, len(recs))
	for _, rec := range recs {
		in, err := fm.interventionFromRecord(rec)
		if err != nil {
			return nil, "", err
		}
		out = append(out, in)
	}
	return out, next, nil
}

// DecideIntervention resolves a pending intervention with a human's
// decision. the decider can never be the key or actor that raised it, and
// must be a named actor when the environment's policy requires one; both
// refusals wrap hitl.ErrSelfApproval or hitl.ErrActorRequired.
func (fm *FleetManager) DecideIntervention(ctx context.Context, id string, req DecideRequest) (hitl.Intervention, error) {
	in, err := fm.resolveIntervention(ctx, id, auditMeta{remoteAddr: req.RemoteAddr}, func(in hitl.Intervention, now time.Time) (hitl.Intervention, error) {
		// a decision that arrives after the deadline loses to it: the
		// caller was promised on_timeout at the deadline.
		if hitl.Expired(now, in.Deadline) {
			return hitl.Expire(in, now)
		}
		if in.State == hitl.StatePending {
			if err := hitl.CheckDecider(in.TriggeredBy, req.By, fm.requireActor(in.EnvID)); err != nil {
				return in, err
			}
		}
		signal := in.Kind == hitl.KindStall || in.Kind == hitl.KindExpiry
		if signal && (req.EditedArgv != nil || req.EditedText != nil || req.Grant != "") {
			return in, fmt.Errorf("%w: edited_argv and grant do not apply to a %s signal", ErrInvalidIntervention, in.Kind)
		}
		if req.Extend != 0 && (in.Kind != hitl.KindExpiry || req.Outcome != hitl.OutcomeApprove) {
			return in, fmt.Errorf("%w: extend only applies to approving an expiry", ErrInvalidIntervention)
		}
		var (
			out hitl.Intervention
			err error
		)
		if in.Kind == hitl.KindExpiry && req.Outcome == hitl.OutcomeApprove {
			extend := req.Extend
			if extend == 0 {
				extend = fm.defaultExtend(in)
			}
			out, err = hitl.Extend(in, extend, req.By, req.Reason, now)
		} else {
			out, err = hitl.Decide(in, req.Outcome, req.Answer, req.Reason, req.By, now)
		}
		var resolved *hitl.ErrResolved
		if err != nil && !errors.As(err, &resolved) {
			return in, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
		}
		if err != nil || signal {
			return out, err
		}
		if out.Decision.Outcome == hitl.OutcomeApprove {
			if err := applyGateDecision(&out, fm.hitlSpec(in.EnvID), gateEdit{argv: req.EditedArgv, text: req.EditedText}, req.Grant); err != nil {
				return in, err
			}
		} else if req.EditedArgv != nil || req.EditedText != nil || req.Grant != "" {
			return in, fmt.Errorf("%w: edited_argv, edited_text and grant only apply to an approval", ErrInvalidIntervention)
		}
		return out, nil
	})
	if err != nil {
		return in, err
	}
	if in.State == hitl.StateExpired {
		return in, fmt.Errorf("%w: intervention %s expired before the decision arrived", ErrInterventionResolved, id)
	}
	fm.storeGrant(ctx, in, req.RemoteAddr)
	fm.applyExtension(in)
	fm.touchActivity(in.EnvID)
	return in, nil
}

// WaitIntervention blocks until the intervention is resolved, maxWait
// passes, or ctx is done, and returns its state at that point. a maxWait of
// zero is a plain read.
func (fm *FleetManager) WaitIntervention(ctx context.Context, id string, maxWait time.Duration) (hitl.Intervention, error) {
	until := time.Now().Add(maxWait)
	for {
		// register before reading so a resolve between the read and the
		// select still wakes us.
		wake := fm.interventionWaiter(id)
		in, err := fm.GetIntervention(ctx, id)
		if err != nil || in.Settled() {
			return in, err
		}
		wait := time.Until(until)
		if wait <= 0 {
			return in, nil
		}
		// wake at the deadline too, so expiry is applied on time. a
		// resolved one waiting on its run only wakes on the result.
		if d := time.Until(in.Deadline); in.State == hitl.StatePending && d < wait {
			wait = max(d, 10*time.Millisecond)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return in, ctx.Err()
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// requireActor reports whether an environment's policy sets require_actor.
// an environment that is gone has no pending interventions left to decide.
func (fm *FleetManager) requireActor(envID string) bool {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[envID]
	return ok && v.spec.Hitl != nil && v.spec.Hitl.RequireActor
}

// cancelEnvInterventions cancels every pending intervention of an
// environment that can no longer answer them. v is the environment, already
// removed from the fleet, so its webhooks can still be told.
func (fm *FleetManager) cancelEnvInterventions(ctx context.Context, v *vm, envID, reason string) {
	meta := auditMeta{targets: fm.webhookTargets(v)}
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{EnvID: envID, State: string(hitl.StatePending)})
	if err != nil {
		fm.logger.Warn("list pending interventions for cancel failed", "vm", envID, "err", err)
		return
	}
	for _, rec := range recs {
		if _, err := fm.resolveIntervention(ctx, rec.ID, meta, func(in hitl.Intervention, now time.Time) (hitl.Intervention, error) {
			return hitl.Cancel(in, reason, now)
		}); err != nil && !errors.Is(err, ErrInterventionResolved) {
			fm.logger.Warn("cancel intervention failed", "intervention", rec.ID, "vm", envID, "err", err)
		}
	}
}

// moveInterventions re-homes pending interventions from a migrate source to
// its target: a migrate is the same logical environment under a new id, so
// a human's pending decision follows it. resolved ones stay where they were
// raised.
func (fm *FleetManager) moveInterventions(ctx context.Context, fromID, toID string) {
	fm.hitlMu.Lock()
	defer fm.hitlMu.Unlock()
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{EnvID: fromID, State: string(hitl.StatePending)})
	if err != nil {
		fm.logger.Warn("list pending interventions for migrate failed", "vm", fromID, "err", err)
		return
	}
	for _, rec := range recs {
		rec.EnvID = toID
		rec.UpdatedAt = time.Now()
		if err := fm.store.UpsertIntervention(ctx, rec); err != nil {
			fm.logger.Warn("move intervention failed", "intervention", rec.ID, "from", fromID, "to", toID, "err", err)
		}
	}
	// grants follow the logical environment the same way.
	if err := fm.store.MoveHitlGrants(ctx, fromID, toID); err != nil {
		fm.logger.Warn("move hitl grants failed", "from", fromID, "to", toID, "err", err)
	}
	// a held takeover follows the environment too.
	if l, ok := fm.hitlLocks[fromID]; ok {
		delete(fm.hitlLocks, fromID)
		fm.hitlLocks[toID] = l
	}
}

func (fm *FleetManager) expireIntervention(ctx context.Context, id string) (hitl.Intervention, error) {
	in, err := fm.resolveIntervention(ctx, id, auditMeta{}, func(in hitl.Intervention, now time.Time) (hitl.Intervention, error) {
		return hitl.Expire(in, now)
	})
	if errors.Is(err, ErrInterventionResolved) {
		// someone else resolved it first; that state is the answer.
		return in, nil
	}
	return in, err
}

// resolveIntervention applies one transition under hitlMu: read the current
// record, transition it, write it back, then wake waiters and publish. it
// returns the stored intervention and ErrInterventionResolved when it was
// already terminal.
func (fm *FleetManager) resolveIntervention(ctx context.Context, id string, meta auditMeta, apply func(hitl.Intervention, time.Time) (hitl.Intervention, error)) (hitl.Intervention, error) {
	fm.hitlMu.Lock()
	rec, err := fm.store.GetIntervention(ctx, id)
	if err != nil {
		fm.hitlMu.Unlock()
		return hitl.Intervention{}, err
	}
	current, err := fm.interventionFromRecord(rec)
	if err != nil {
		fm.hitlMu.Unlock()
		return hitl.Intervention{}, err
	}
	next, err := apply(current, time.Now())
	if err != nil {
		fm.hitlMu.Unlock()
		var resolved *hitl.ErrResolved
		if errors.As(err, &resolved) {
			return current, fmt.Errorf("%w: %w", ErrInterventionResolved, err)
		}
		return current, err
	}
	out, err := fm.interventionToRecord(next)
	if err == nil {
		err = fm.store.UpsertIntervention(ctx, out)
	}
	if err != nil {
		fm.hitlMu.Unlock()
		return current, err
	}
	if ch, ok := fm.hitlWaiters[id]; ok {
		close(ch)
		delete(fm.hitlWaiters, id)
	}
	run := fm.claimRun(next)
	fm.dropLockLocked(next.EnvID, id)
	fm.hitlMu.Unlock()
	if run {
		fm.launchRun(next)
	}

	fm.appendEvent(ctx, "intervention", id, "intervention."+string(next.State), map[string]any{
		"env_id": next.EnvID, "outcome": string(next.Decision.Outcome), "decided_by": next.Decision.DecidedBy.ID,
	})
	fm.publishInterventionEvent(next)
	fm.auditIntervention(ctx, next, meta)
	return next, nil
}

// interventionWaiter returns a channel closed when id is next resolved.
func (fm *FleetManager) interventionWaiter(id string) <-chan struct{} {
	fm.hitlMu.Lock()
	defer fm.hitlMu.Unlock()
	if fm.hitlWaiters == nil {
		fm.hitlWaiters = make(map[string]chan struct{})
	}
	ch, ok := fm.hitlWaiters[id]
	if !ok {
		ch = make(chan struct{})
		fm.hitlWaiters[id] = ch
	}
	return ch
}

// publishInterventionEvent sends a content-free hint on the environment's
// event stream. the stream drops events under load, so this is a nudge to
// re-read the intervention, never the record itself.
func (fm *FleetManager) publishInterventionEvent(in hitl.Intervention) {
	if fm.broadcaster == nil {
		return
	}
	fm.mu.RLock()
	v, ok := fm.vms[in.EnvID]
	var info VMInfo
	if ok {
		info = v.toInfo()
	}
	fm.mu.RUnlock()
	if !ok {
		return
	}
	fm.broadcaster.publish(EnvironmentEvent{
		ID:        NewEventID(),
		Kind:      "intervention",
		VMID:      in.EnvID,
		State:     info.State,
		URL:       info.URL,
		UpdatedAt: in.UpdatedAt,
		Intervention: &InterventionEvent{
			ID:    in.ID,
			Kind:  string(in.Kind),
			State: string(in.State),
		},
	})
}

// interventionResult is the sealed result column: an ask's answer, or what
// running an approved gated action produced.
type interventionResult struct {
	Answer string       `json:"answer,omitempty"`
	Run    *hitl.Result `json:"run,omitempty"`
}

// interventionPayload is the sealed payload column. the ask fields keep the
// shape rows written before gates existed already have.
type interventionPayload struct {
	hitl.AskPayload
	Mode      hitl.Mode              `json:"mode,omitempty"`
	Exec      *hitl.ExecPayload      `json:"exec,omitempty"`
	Lifecycle *hitl.LifecyclePayload `json:"lifecycle,omitempty"`
	Secret    *hitl.SecretPayload    `json:"secret,omitempty"`
	Takeover  *hitl.TakeoverPayload  `json:"takeover,omitempty"`
	Signal    *hitl.SignalPayload    `json:"signal,omitempty"`
	Computer  *hitl.ComputerPayload  `json:"computer,omitempty"`
	Review    *hitl.ReviewPayload    `json:"review,omitempty"`
	Decider   *hitl.DeciderNote      `json:"decider,omitempty"`
}

// interventionEdit is the sealed edited_payload column: what the decider
// changed about a gated action.
type interventionEdit struct {
	Argv  []string `json:"argv,omitempty"`
	Text  *string  `json:"text,omitempty"`
	Grant string   `json:"grant,omitempty"`
}

func (fm *FleetManager) seal(b []byte) ([]byte, bool, error) {
	if len(fm.tokenEncryptionKey) != 32 || b == nil {
		return b, false, nil
	}
	ct, err := secrets.EncryptBytes(b, fm.tokenEncryptionKey)
	return ct, true, err
}

func (fm *FleetManager) unseal(b []byte, sealed bool) ([]byte, error) {
	if !sealed || b == nil {
		return b, nil
	}
	if len(fm.tokenEncryptionKey) != 32 {
		return nil, errors.New("intervention is encrypted but no TOKEN_ENCRYPTION_KEY is configured")
	}
	return secrets.DecryptBytes(b, fm.tokenEncryptionKey)
}

func (fm *FleetManager) interventionToRecord(in hitl.Intervention) (InterventionRecord, error) {
	rec := InterventionRecord{
		ID:        in.ID,
		EnvID:     in.EnvID,
		Kind:      string(in.Kind),
		Gate:      in.Gate,
		State:     string(in.State),
		OnTimeout: string(in.OnTimeout),
		Deadline:  in.Deadline,
		CreatedAt: in.CreatedAt,
		UpdatedAt: in.UpdatedAt,
	}
	payload, err := json.Marshal(interventionPayload{
		AskPayload: in.Payload, Mode: in.Mode, Exec: in.Exec, Lifecycle: in.Lifecycle, Secret: in.Secret, Takeover: in.Takeover, Signal: in.Signal,
		Computer: in.Computer, Review: in.Review, Decider: in.Decider,
	})
	if err != nil {
		return rec, err
	}
	triggeredBy, err := json.Marshal(in.TriggeredBy)
	if err != nil {
		return rec, err
	}
	rec.TriggeredBy = triggeredBy

	var result, reason, edit []byte
	if in.Result != nil {
		if result, err = json.Marshal(interventionResult{Run: in.Result}); err != nil {
			return rec, err
		}
	}
	if d := in.Decision; d != nil {
		rec.Outcome = string(d.Outcome)
		decidedBy, err := json.Marshal(d.DecidedBy)
		if err != nil {
			return rec, err
		}
		rec.DecidedBy = decidedBy
		at := d.DecidedAt
		rec.DecidedAt = &at
		if d.Answer != "" {
			if result, err = json.Marshal(interventionResult{Answer: d.Answer, Run: in.Result}); err != nil {
				return rec, err
			}
		}
		if d.EditedArgv != nil || d.EditedText != nil || d.Grant != "" {
			if edit, err = json.Marshal(interventionEdit{Argv: d.EditedArgv, Text: d.EditedText, Grant: d.Grant}); err != nil {
				return rec, err
			}
		}
		if d.Reason != "" {
			reason = []byte(d.Reason)
		}
	}

	if rec.Payload, rec.Sealed, err = fm.seal(payload); err != nil {
		return rec, fmt.Errorf("seal intervention payload: %w", err)
	}
	if rec.EditedPayload, _, err = fm.seal(edit); err != nil {
		return rec, fmt.Errorf("seal intervention edit: %w", err)
	}
	if rec.Result, _, err = fm.seal(result); err != nil {
		return rec, fmt.Errorf("seal intervention result: %w", err)
	}
	if rec.Reason, _, err = fm.seal(reason); err != nil {
		return rec, fmt.Errorf("seal intervention reason: %w", err)
	}
	return rec, nil
}

func (fm *FleetManager) interventionFromRecord(rec InterventionRecord) (hitl.Intervention, error) {
	in := hitl.Intervention{
		ID:        rec.ID,
		EnvID:     rec.EnvID,
		Kind:      hitl.Kind(rec.Kind),
		Gate:      rec.Gate,
		State:     hitl.State(rec.State),
		OnTimeout: hitl.OnTimeout(rec.OnTimeout),
		Deadline:  rec.Deadline,
		CreatedAt: rec.CreatedAt,
		UpdatedAt: rec.UpdatedAt,
	}
	if len(rec.TriggeredBy) > 0 {
		if err := json.Unmarshal(rec.TriggeredBy, &in.TriggeredBy); err != nil {
			return in, fmt.Errorf("intervention %s: triggered_by: %w", rec.ID, err)
		}
	}
	payload, err := fm.unseal(rec.Payload, rec.Sealed)
	if err != nil {
		return in, fmt.Errorf("intervention %s: payload: %w", rec.ID, err)
	}
	if len(payload) > 0 {
		var p interventionPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return in, fmt.Errorf("intervention %s: payload: %w", rec.ID, err)
		}
		in.Payload, in.Mode, in.Exec, in.Lifecycle = p.AskPayload, p.Mode, p.Exec, p.Lifecycle
		in.Secret, in.Takeover, in.Signal = p.Secret, p.Takeover, p.Signal
		in.Computer, in.Review, in.Decider = p.Computer, p.Review, p.Decider
	}
	result, err := fm.unseal(rec.Result, rec.Sealed)
	if err != nil {
		return in, fmt.Errorf("intervention %s: result: %w", rec.ID, err)
	}
	var r interventionResult
	if len(result) > 0 {
		if err := json.Unmarshal(result, &r); err != nil {
			return in, fmt.Errorf("intervention %s: result: %w", rec.ID, err)
		}
		in.Result = r.Run
	}
	if rec.Outcome == "" {
		return in, nil
	}
	d := &hitl.Decision{Outcome: hitl.Outcome(rec.Outcome)}
	if len(rec.DecidedBy) > 0 {
		if err := json.Unmarshal(rec.DecidedBy, &d.DecidedBy); err != nil {
			return in, fmt.Errorf("intervention %s: decided_by: %w", rec.ID, err)
		}
	}
	if rec.DecidedAt != nil {
		d.DecidedAt = *rec.DecidedAt
	}
	d.Answer = r.Answer
	edit, err := fm.unseal(rec.EditedPayload, rec.Sealed)
	if err != nil {
		return in, fmt.Errorf("intervention %s: edited_payload: %w", rec.ID, err)
	}
	if len(edit) > 0 {
		var e interventionEdit
		if err := json.Unmarshal(edit, &e); err != nil {
			return in, fmt.Errorf("intervention %s: edited_payload: %w", rec.ID, err)
		}
		d.EditedArgv, d.EditedText, d.Grant = e.Argv, e.Text, e.Grant
	}
	reason, err := fm.unseal(rec.Reason, rec.Sealed)
	if err != nil {
		return in, fmt.Errorf("intervention %s: reason: %w", rec.ID, err)
	}
	d.Reason = string(reason)
	in.Decision = d
	return in, nil
}
