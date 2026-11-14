package durable

import (
	"context"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opArtifactMove is the op label on a move's step spans and metrics.
const opArtifactMove = "artifact_move"

// movePinTTL is how long a move's pins last if the move never releases them,
// as when its invocation is killed. a move that finishes, fails or is
// cancelled releases them itself; this only stops a dead one holding an
// artifact forever.
const movePinTTL = 24 * time.Hour

// defaultPullAttemptBound is how long one pull attempt can take with no
// ORCH_ARTIFACT_PULL_TIMEOUT_SECONDS: the agent client's 10 minute request
// timeout, three times over for its keyed retries, plus their backoff.
const defaultPullAttemptBound = 35 * time.Minute

// Artifacts is the part of the fleet the move handler drives, one method per
// step. *FleetManager satisfies it.
type Artifacts interface {
	ArtifactMoveCommitted(ctx context.Context, job orchestrator.ArtifactMoveJob) (string, error)
	ArtifactPull(ctx context.Context, job orchestrator.ArtifactMoveJob) (orchestrator.ArtifactMoved, error)
	ArtifactRecord(ctx context.Context, job orchestrator.ArtifactMoveJob, moved orchestrator.ArtifactMoved) (string, error)
	ArtifactDiscard(ctx context.Context, job orchestrator.ArtifactMoveJob) error
	ArtifactPullTimeout() time.Duration
}

// moveAbortTimeout is how long restate lets a move attempt run without
// journal progress before it aborts and retries it. a pull is one long step,
// so it has to outlast one pull attempt.
func moveAbortTimeout(pullTimeout time.Duration) time.Duration {
	bound := defaultPullAttemptBound
	if pullTimeout > 0 {
		bound = pullTimeout
	}
	return bound + 5*time.Minute
}

// move is Artifacts/{local_id}.Move. keying on the destination's snapshot id
// runs one move into a given copy at a time; a second one finds the first's
// copy recorded and returns it. the pins on both ends live in ArtifactPin
// state for the whole move, so gc leaves them alone across a restart. a pull
// or record that fails for good deletes what reached the destination, so no
// copy is left there that nothing records.
func move(ctx restate.ObjectContext, fleet Artifacts, policy RetryPolicy, job orchestrator.ArtifactMoveJob) (string, error) {
	if key := restate.Key(ctx); job.LocalID != key {
		return "", restate.TerminalErrorf("move into %s sent to Artifacts/%s", job.LocalID, key)
	}

	committed, err := Run(ctx, opArtifactMove, "check", func(c context.Context) (string, error) {
		return fleet.ArtifactMoveCommitted(c, job)
	})
	if err != nil || committed != "" {
		return committed, err
	}

	holder := "move:" + ctx.Request().ID
	ids := job.PinIDs()
	for _, id := range ids {
		if _, err := restate.Object[restate.Void](ctx, ArtifactPinObject, id, "Pin").
			Request(PinRequest{Holder: holder, TTL: movePinTTL}); err != nil {
			return "", err
		}
	}

	retry := []restate.RunOption{
		restate.WithInitialRetryInterval(policy.InitialInterval),
		restate.WithMaxRetryInterval(policy.MaxInterval),
		restate.WithMaxRetryAttempts(uint(policy.MaxAttempts)),
	}
	moved, err := Run(ctx, opArtifactMove, "pull", func(c context.Context) (orchestrator.ArtifactMoved, error) {
		moved, err := fleet.ArtifactPull(c, job)
		if err != nil && orchestrator.PermanentArtifactError(err) {
			return moved, restate.ToTerminalError(err)
		}
		return moved, err
	}, retry...)
	var id string
	if err == nil {
		id, err = Run(ctx, opArtifactMove, "record", func(c context.Context) (string, error) {
			return fleet.ArtifactRecord(c, job, moved)
		}, retry...)
	}
	if err != nil {
		if !restate.IsTerminalError(err) {
			// suspension: restate resumes the move here.
			return "", err
		}
		if _, derr := Run(ctx, opArtifactMove, "discard", func(c context.Context) (restate.Void, error) {
			return restate.Void{}, fleet.ArtifactDiscard(c, job)
		}, retry...); derr != nil && !restate.IsTerminalError(derr) {
			return "", derr
		}
		releasePins(ctx, holder, ids)
		return "", err
	}
	releasePins(ctx, holder, ids)
	return id, nil
}

func releasePins(ctx restate.Context, holder string, ids []string) {
	for _, id := range ids {
		restate.ObjectSend(ctx, ArtifactPinObject, id, "Release").Send(holder)
	}
}

// PinRequest holds an ArtifactPin for Holder, until released or TTL passes.
type PinRequest struct {
	Holder string        `json:"holder"`
	TTL    time.Duration `json:"ttl"`
}

// pinExpiry is the delayed send that lapses one pin. Deadline tells a lapse
// apart from one a later Pin by the same holder has already extended.
type pinExpiry struct {
	Holder   string `json:"holder"`
	Deadline int64  `json:"deadline"`
}

// pinHoldersKey is the ArtifactPin state: holder to deadline, unix ms.
const pinHoldersKey = "holders"

// pin is ArtifactPin/{snapshot_id}.Pin. the lapse is a delayed send rather
// than a sleeping invocation, so a pin held for hours pins nothing to this
// deployment.
func pin(ctx restate.ObjectContext, req PinRequest) error {
	if req.Holder == "" || req.TTL <= 0 {
		return restate.TerminalErrorf("pin needs a holder and a positive ttl")
	}
	now, err := restate.Run(ctx, func(restate.RunContext) (int64, error) {
		return time.Now().UnixMilli(), nil
	}, restate.WithName("now"))
	if err != nil {
		return err
	}
	holders, err := restate.Get[map[string]int64](ctx, pinHoldersKey)
	if err != nil {
		return err
	}
	if holders == nil {
		holders = map[string]int64{}
	}
	deadline := now + req.TTL.Milliseconds()
	holders[req.Holder] = deadline
	restate.Set(ctx, pinHoldersKey, holders)
	restate.ObjectSend(ctx, ArtifactPinObject, restate.Key(ctx), "Expire").
		Send(pinExpiry{Holder: req.Holder, Deadline: deadline}, restate.WithDelay(req.TTL))
	return nil
}

// release is ArtifactPin/{snapshot_id}.Release.
func release(ctx restate.ObjectContext, holder string) error {
	return dropHolder(ctx, holder, 0)
}

// expire is ArtifactPin/{snapshot_id}.Expire, the pin's timer.
func expire(ctx restate.ObjectContext, e pinExpiry) error {
	return dropHolder(ctx, e.Holder, e.Deadline)
}

// dropHolder removes holder, or with a deadline only the pin that deadline
// belongs to. the last holder out clears the object.
func dropHolder(ctx restate.ObjectContext, holder string, deadline int64) error {
	holders, err := restate.Get[map[string]int64](ctx, pinHoldersKey)
	if err != nil {
		return err
	}
	got, ok := holders[holder]
	if !ok || (deadline != 0 && got != deadline) {
		return nil
	}
	delete(holders, holder)
	if len(holders) == 0 {
		restate.ClearAll(ctx)
		return nil
	}
	restate.Set(ctx, pinHoldersKey, holders)
	return nil
}

// pinned is ArtifactPin/{snapshot_id}.Pinned.
func pinned(ctx restate.ObjectSharedContext) (bool, error) {
	holders, err := restate.Get[map[string]int64](ctx, pinHoldersKey)
	if err != nil {
		return false, err
	}
	return len(holders) > 0, nil
}

// MoveArtifact runs Artifacts/{job.LocalID}.Move and waits for it. a move
// takes as long as its bytes do, so it goes through the client with no
// request timeout of its own; ctx bounds the wait, and the move keeps running
// if ctx ends first.
func (c *Client) MoveArtifact(ctx context.Context, job orchestrator.ArtifactMoveJob) (string, error) {
	return ingress.Object[orchestrator.ArtifactMoveJob, string](c.longIngress, ArtifactsObject, job.LocalID, "Move").
		Request(ctx, job)
}

// PinArtifacts pins each id for holder, for at most ttl.
func (c *Client) PinArtifacts(ctx context.Context, holder string, ttl time.Duration, ids ...string) error {
	for _, id := range ids {
		if _, err := ingress.Object[PinRequest, restate.Void](c.ingress, ArtifactPinObject, id, "Pin").
			Request(ctx, PinRequest{Holder: holder, TTL: ttl}); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseArtifacts drops holder's pin on each id.
func (c *Client) ReleaseArtifacts(ctx context.Context, holder string, ids ...string) error {
	for _, id := range ids {
		if _, err := ingress.Object[string, restate.Void](c.ingress, ArtifactPinObject, id, "Release").
			Request(ctx, holder); err != nil {
			return err
		}
	}
	return nil
}

// ArtifactPinned reports whether any holder pins id.
func (c *Client) ArtifactPinned(ctx context.Context, id string) (bool, error) {
	return ingress.Object[restate.Void, bool](c.ingress, ArtifactPinObject, id, "Pinned").
		Request(ctx, restate.Void{})
}

var _ orchestrator.DurableArtifacts = (*Client)(nil)
