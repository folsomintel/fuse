package durable

import (
	"context"
	"errors"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opRotateToken is the op label on token rotation's step spans and metrics.
const opRotateToken = "rotate_token"

// rotateRefusedCode is the error code a token rotation's own refusal (vm not
// running, no encryption key) crosses restate as, so the client hands the api
// the fleet's message unchanged. 480-486 are the snapshot sentinels.
const rotateRefusedCode restate.Code = 487

// Tokens is the part of the fleet the token rotation handler drives, one
// method per step. *FleetManager satisfies it.
type Tokens interface {
	RotateTokenGenerate(ctx context.Context, vmID string) (orchestrator.RotatedCredentials, error)
	RotateTokenUpload(ctx context.Context, vmID string, rotated orchestrator.RotatedCredentials) error
	RotateTokenPersist(ctx context.Context, vmID string, rotated orchestrator.RotatedCredentials) error
}

// rotatePermanent marks err terminal when a retry cannot change it: the
// rotation's own refusals under rotateRefusedCode, the rest the way snapshot
// steps mark theirs (vm not found keeps its sentinel, an agent 4xx is final).
func rotatePermanent(err error) error {
	if orchestrator.TokenRotationRefused(err) {
		return restate.ToTerminalError(err, restate.WithErrorCode(rotateRefusedCode))
	}
	return permanent(err)
}

// rotateToken is Environment/{vm_id}.RotateToken: generate, upload, persist.
// generate is journaled, so a replay after a crash uploads and persists the
// token it already made instead of a second one, and the stored token ends up
// the one in the guest. its journaled value is ciphertext only: the plaintext
// token is decrypted inside the upload and persist steps and never returned.
func rotateToken(ctx restate.ObjectContext, fleet Tokens, policy RetryPolicy) error {
	vmID := restate.Key(ctx)
	retry := retryUnder(policy)
	rotated, err := Run(ctx, opRotateToken, "generate", func(c context.Context) (orchestrator.RotatedCredentials, error) {
		rotated, err := fleet.RotateTokenGenerate(c, vmID)
		return rotated, rotatePermanent(err)
	})
	if err != nil {
		return err
	}
	if err := runStep(ctx, opRotateToken, "upload", func(c context.Context) error {
		return rotatePermanent(fleet.RotateTokenUpload(c, vmID, rotated))
	}, retry...); err != nil {
		return err
	}
	return runStep(ctx, opRotateToken, "persist", func(c context.Context) error {
		return rotatePermanent(fleet.RotateTokenPersist(c, vmID, rotated))
	}, retry...)
}

// RotateToken runs Environment/{vmID}.RotateToken and waits for it. the
// rotation may queue behind a long snapshot on the same vm, so it goes through
// the client with no request timeout of its own; ctx bounds the wait, and the
// rotation keeps running if ctx ends first.
func (c *Client) RotateToken(ctx context.Context, vmID string) error {
	_, err := ingress.Object[restate.Void, restate.Void](c.longIngress, EnvironmentObject, vmID, "RotateToken").
		Request(ctx, restate.Void{})
	var ierr ingress.Error
	if errors.As(err, &ierr) && ierr.Code() == rotateRefusedCode {
		return errors.New(ierr.Message())
	}
	return fleetError(err)
}

var _ orchestrator.DurableTokenRotator = (*Client)(nil)
