package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

var (
	// ErrNoTakeover is a release or a takeover shell on an environment no
	// human holds.
	ErrNoTakeover = errors.New("no takeover held")

	// ErrTakeoverHeld is a second human taking over an environment someone
	// already holds.
	ErrTakeoverHeld = hitl.ErrTakeoverHeld

	// ErrNotTakeoverHolder is a takeover shell opened by someone other than
	// the human holding the takeover.
	ErrNotTakeoverHolder = errors.New("not the takeover holder")
)

// TakeoverRequest is a human taking control of an environment.
type TakeoverRequest struct {
	Via hitl.TakeoverVia
	// Reason is recorded when the human starts the takeover themselves.
	Reason     string
	By         hitl.Actor
	RemoteAddr string
}

// StartTakeover gives a human control of an environment and locks the
// agent out of exec and computer calls until release or the deadline. it
// claims the agent's pending takeover request when there is one (the agent
// may not claim its own), and otherwise opens a takeover on the human's
// behalf. taking over again as the current holder returns the same one.
func (fm *FleetManager) StartTakeover(ctx context.Context, envID string, req TakeoverRequest) (hitl.Intervention, error) {
	fm.mu.RLock()
	v, ok := fm.vms[envID]
	var (
		state VMState
		spec  *hitl.Spec
	)
	if ok {
		state, spec = v.state, v.spec.Hitl
	}
	fm.mu.RUnlock()
	switch {
	case !ok:
		return hitl.Intervention{}, fmt.Errorf("%w: %s", ErrVMNotFound, envID)
	case state != VMStateRunning:
		return hitl.Intervention{}, fmt.Errorf("%w: vm %s is in state %s", ErrVMNotRunning, envID, state)
	case spec == nil || spec.Requests.Takeover == nil:
		return hitl.Intervention{}, fmt.Errorf("%w: environment %s does not declare hitl.requests.takeover", ErrHitlNotDeclared, envID)
	}
	via := req.Via
	if via == "" {
		via = hitl.ViaTerminal
	}
	if !spec.Requests.Takeover.Allows(via) {
		return hitl.Intervention{}, fmt.Errorf("%w: environment %s does not allow takeover via %s", ErrHitlNotDeclared, envID, via)
	}
	if err := hitl.ValidateTakeoverReason(req.Reason); err != nil {
		return hitl.Intervention{}, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
	}

	fm.hitlMu.Lock()
	in, raised, err := fm.startTakeoverLocked(ctx, envID, *spec, via, req)
	if err == nil {
		fm.setLockLocked(in)
	}
	fm.hitlMu.Unlock()
	if err != nil {
		return in, err
	}

	if raised {
		fm.appendEvent(ctx, "intervention", in.ID, "intervention.raised", map[string]any{
			"env_id": envID, "kind": string(in.Kind), "deadline": in.Deadline,
		})
		fm.auditIntervention(ctx, in, auditMeta{remoteAddr: req.RemoteAddr})
	}
	fm.appendEvent(ctx, "intervention", in.ID, "intervention.takeover_started", map[string]any{
		"env_id": envID, "holder": req.By.ID,
	})
	fm.publishInterventionEvent(in)
	fm.recordAudit(ctx, hitl.AuditEntry{
		Event: hitl.AuditTakeoverStarted, InterventionID: in.ID, EnvID: envID, Actor: req.By, RemoteAddr: req.RemoteAddr,
	}, fm.webhookTargetsFor(envID), &in)
	fm.touchActivity(envID)
	return in, nil
}

// startTakeoverLocked finds or opens the takeover and marks it held. raised
// is true when it opened a new one. hitlMu must be held.
func (fm *FleetManager) startTakeoverLocked(ctx context.Context, envID string, spec hitl.Spec, via hitl.TakeoverVia, req TakeoverRequest) (hitl.Intervention, bool, error) {
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{EnvID: envID, State: string(hitl.StatePending)})
	if err != nil {
		return hitl.Intervention{}, false, err
	}
	now := time.Now()
	var claim *hitl.Intervention
	for _, rec := range recs {
		if rec.Kind != string(hitl.KindTakeover) || hitl.Expired(now, rec.Deadline) {
			continue
		}
		in, err := fm.interventionFromRecord(rec)
		if err != nil {
			return hitl.Intervention{}, false, err
		}
		if in.Takeover.Held() {
			if in.Takeover.Holder.ID == req.By.ID {
				return in, false, nil
			}
			return in, false, fmt.Errorf("%w: environment %s is held by %s (intervention %s)", ErrTakeoverHeld, envID, in.Takeover.Holder.ID, in.ID)
		}
		if claim == nil {
			claim = &in
		}
	}

	raised := false
	var in hitl.Intervention
	if claim != nil {
		// an agent asked for this; the agent may not take its own request.
		if err := hitl.CheckDecider(claim.TriggeredBy, req.By, spec.RequireActor); err != nil {
			return *claim, false, err
		}
		in = *claim
		// the human picks the way in when they name one: an agent asking
		// for help does not know whether a shell or the desktop fits.
		if req.Via != "" && in.Takeover.Via != via {
			t := *in.Takeover
			t.Via = via
			in.Takeover = &t
		}
	} else {
		in = hitl.Intervention{
			ID:          hitl.NewID(now),
			EnvID:       envID,
			Kind:        hitl.KindTakeover,
			State:       hitl.StatePending,
			Takeover:    &hitl.TakeoverPayload{Via: via, Reason: req.Reason},
			TriggeredBy: req.By,
			Deadline:    hitl.Deadline(now, spec, 0),
			OnTimeout:   hitl.OnTimeoutReject,
			CreatedAt:   now,
		}
		raised = true
	}
	in, err = hitl.StartTakeover(in, req.By, now)
	if err != nil {
		return in, false, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
	}
	rec, err := fm.interventionToRecord(in)
	if err == nil {
		err = fm.store.UpsertIntervention(ctx, rec)
	}
	return in, raised, err
}

// ReleaseTakeover ends the held takeover on an environment and hands the
// note back to the agent. any human allowed to take over may release, so a
// takeover whose holder walked away does not have to wait out its
// deadline.
func (fm *FleetManager) ReleaseTakeover(ctx context.Context, envID, note string, by hitl.Actor, remoteAddr string) (hitl.Intervention, error) {
	in, err := fm.heldTakeover(ctx, envID)
	if err != nil {
		return in, err
	}
	out, err := fm.resolveIntervention(ctx, in.ID, auditMeta{remoteAddr: remoteAddr}, func(in hitl.Intervention, now time.Time) (hitl.Intervention, error) {
		if hitl.Expired(now, in.Deadline) {
			return hitl.Expire(in, now)
		}
		out, err := hitl.ReleaseTakeover(in, note, by, now)
		var resolved *hitl.ErrResolved
		if err != nil && !errors.As(err, &resolved) {
			return in, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
		}
		return out, err
	})
	if err != nil {
		return out, err
	}
	if out.State == hitl.StateExpired {
		return out, fmt.Errorf("%w: takeover %s expired before the release arrived", ErrInterventionResolved, out.ID)
	}
	fm.touchActivity(envID)
	return out, nil
}

// heldTakeover returns the takeover a human holds on an environment.
func (fm *FleetManager) heldTakeover(ctx context.Context, envID string) (hitl.Intervention, error) {
	fm.hitlMu.Lock()
	l, ok := fm.hitlLocks[envID]
	fm.hitlMu.Unlock()
	if !ok || l.Kind != hitl.KindTakeover {
		return hitl.Intervention{}, fmt.Errorf("%w: nobody holds environment %s", ErrNoTakeover, envID)
	}
	in, err := fm.GetIntervention(ctx, l.InterventionID)
	if err != nil {
		return in, err
	}
	if !in.Locks() {
		return in, fmt.Errorf("%w: takeover %s is %s", ErrNoTakeover, in.ID, in.State)
	}
	return in, nil
}

// TakeoverAttach opens the takeover holder's shell. it is the ordinary
// attach path plus a recording: the session is saved as an encrypted cast
// when it closes, linked from a takeover_session audit row, and the first
// thing the human sees is a banner saying so. master skips the holder
// check, as the master token can attach anyway.
func (fm *FleetManager) TakeoverAttach(ctx context.Context, id string, spec AttachSpec, by hitl.Actor, master bool, remoteAddr string) (io.ReadWriteCloser, error) {
	in, err := fm.GetIntervention(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Kind != hitl.KindTakeover {
		return nil, fmt.Errorf("%w: intervention %s is a %s, not a takeover", ErrInvalidIntervention, id, in.Kind)
	}
	if !in.Locks() {
		return nil, fmt.Errorf("%w: takeover %s is not held (state %s); start it with ?action=takeover first", ErrNoTakeover, id, in.State)
	}
	if !master && in.Takeover.Holder.ID != by.ID {
		return nil, fmt.Errorf("%w: takeover %s is held by %s", ErrNotTakeoverHolder, id, in.Takeover.Holder.ID)
	}
	stream, err := fm.Attach(ctx, in.EnvID, spec)
	if err != nil {
		return nil, err
	}
	return newRecordedStream(stream, in, by, spec, func(c cast) {
		fm.saveRecording(context.WithoutCancel(ctx), in, by, remoteAddr, recordingTerminal, c)
	}), nil
}

// TakeoverDesktop opens the takeover holder's live desktop: the computer
// stream plus a recording, an input-action log and a screenshot every
// desktopShotInterval, saved sealed when the view closes and linked from a
// takeover_session audit row. it is the one desktop stream a held
// environment still serves; the plain one answers 423 like the agent's
// clicks do.
func (fm *FleetManager) TakeoverDesktop(ctx context.Context, id string, by hitl.Actor, master bool, remoteAddr string) (io.ReadWriteCloser, error) {
	in, err := fm.GetIntervention(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Kind != hitl.KindTakeover {
		return nil, fmt.Errorf("%w: intervention %s is a %s, not a takeover", ErrInvalidIntervention, id, in.Kind)
	}
	if !in.Locks() {
		return nil, fmt.Errorf("%w: takeover %s is not held (state %s); start it with ?action=takeover first", ErrNoTakeover, id, in.State)
	}
	if in.Takeover.Via != hitl.ViaDesktop {
		return nil, fmt.Errorf("%w: takeover %s is via %s; start one with via desktop", ErrInvalidIntervention, id, in.Takeover.Via)
	}
	if !master && in.Takeover.Holder.ID != by.ID {
		return nil, fmt.Errorf("%w: takeover %s is held by %s", ErrNotTakeoverHolder, id, in.Takeover.Holder.ID)
	}
	stream, err := fm.openComputerStream(ctx, in.EnvID)
	if err != nil {
		return nil, err
	}
	shoot := func(ctx context.Context) (string, error) {
		res, err := fm.guestComputer(ctx, in.EnvID, http.MethodGet, "/v1/computer/screenshot", nil)
		if err != nil {
			return "", err
		}
		if res.Status != http.StatusOK {
			return "", fmt.Errorf("screenshot: guest answered %d", res.Status)
		}
		var shot hitl.ComputerResult
		if err := json.Unmarshal(res.Body, &shot); err != nil {
			return "", err
		}
		return shot.Screenshot, nil
	}
	start := desktopRecording{InterventionID: in.ID, EnvID: in.EnvID, Actor: by.ID}
	return newDesktopRecorder(stream, start, desktopShotInterval, shoot, func(r desktopRecording) {
		data, err := json.Marshal(r)
		if err != nil {
			fm.logger.Error("encode desktop recording failed", "intervention", in.ID, "err", err)
			return
		}
		fm.saveRecording(context.WithoutCancel(ctx), in, by, remoteAddr, recordingDesktop,
			cast{data: data, startedAt: r.StartedAt, endedAt: r.EndedAt, truncated: r.Truncated})
	}), nil
}
