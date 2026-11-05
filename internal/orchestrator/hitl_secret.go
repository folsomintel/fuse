package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// hitlEnvDir holds env-var secrets in the guest, one root-only file per
// variable named after it. it sits under the reserved /fuse directory, which
// the host agent writes 0600 and no api caller can address.
const hitlEnvDir = "/fuse/hitl-env.d"

// hitlEnvWrapper exports every delivered env secret ahead of an agent exec.
// exec runs as a non-login shell that reads neither /fuse/env nor
// profile.d, so the variables are loaded here, per call, from the files.
const hitlEnvWrapper = `for f in ` + hitlEnvDir + `/*; do [ -f "$f" ] && export "${f##*/}=$(cat "$f")"; done; exec "$@"`

// SecretAnswerRequest is a human answering a secret request.
type SecretAnswerRequest struct {
	// Value goes straight into the guest. it is never stored, logged,
	// returned or published.
	Value      string
	By         hitl.Actor
	RemoteAddr string
}

// AnswerSecret writes a human's value into the declared guest target and
// resolves the request. the agent gets back hitl.SecretAnswer; the record
// and the audit row hold only who answered and when.
func (fm *FleetManager) AnswerSecret(ctx context.Context, id string, req SecretAnswerRequest) (hitl.Intervention, error) {
	if len(fm.tokenEncryptionKey) != 32 {
		return hitl.Intervention{}, fmt.Errorf("%w: refusing to answer a secret request", ErrHitlNeedsKey)
	}
	if err := hitl.ValidateSecretValue(req.Value); err != nil {
		return hitl.Intervention{}, fmt.Errorf("%w: %w", ErrInvalidIntervention, err)
	}
	in, err := fm.GetIntervention(ctx, id)
	if err != nil {
		return hitl.Intervention{}, err
	}
	if in.Kind != hitl.KindSecret || in.Secret == nil {
		return in, fmt.Errorf("%w: intervention %s is a %s request; only secret requests take a value", ErrInvalidIntervention, id, in.Kind)
	}
	if in.State.Terminal() {
		return in, fmt.Errorf("%w: intervention %s is already %s", ErrInterventionResolved, id, in.State)
	}
	if err := hitl.CheckDecider(in.TriggeredBy, req.By, fm.requireActor(in.EnvID)); err != nil {
		return in, err
	}

	// the target is looked up in the environment's policy again rather
	// than trusted from the record, so a value only ever lands somewhere
	// the policy declares right now.
	target, ok := fm.secretTarget(in.EnvID, in.Secret.Target)
	if !ok {
		return in, fmt.Errorf("%w: secret target %q is no longer declared", ErrHitlNotDeclared, in.Secret.Target)
	}

	// one answer at a time: a second human answering the same request
	// while the first value is still being written gets a conflict, not a
	// second write.
	fm.hitlMu.Lock()
	if fm.hitlAnswering[id] {
		fm.hitlMu.Unlock()
		return in, fmt.Errorf("%w: intervention %s is being answered", ErrInterventionResolved, id)
	}
	if fm.hitlAnswering == nil {
		fm.hitlAnswering = make(map[string]bool)
	}
	fm.hitlAnswering[id] = true
	fm.hitlMu.Unlock()
	defer func() {
		fm.hitlMu.Lock()
		delete(fm.hitlAnswering, id)
		fm.hitlMu.Unlock()
	}()

	if err := fm.deliverSecret(ctx, in.EnvID, target, req.Value); err != nil {
		// the transport error can echo what it was sent; never let the
		// value out through it.
		msg := strings.ReplaceAll(err.Error(), req.Value, "[REDACTED]")
		return in, fmt.Errorf("deliver secret for intervention %s: %s", id, msg)
	}

	// the value is in the guest now, so the request is answered even if
	// its deadline passed while it was being written.
	out, err := fm.resolveIntervention(ctx, id, auditMeta{remoteAddr: req.RemoteAddr}, func(in hitl.Intervention, now time.Time) (hitl.Intervention, error) {
		return hitl.AnswerSecret(in, req.By, now)
	})
	if err != nil {
		return out, err
	}
	fm.touchActivity(out.EnvID)
	return out, nil
}

// secretTarget returns a declared target of an environment's policy.
func (fm *FleetManager) secretTarget(envID, name string) (hitl.SecretTarget, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[envID]
	if !ok || v.spec.Hitl == nil {
		return hitl.SecretTarget{}, false
	}
	return v.spec.Hitl.Requests.Secret.Target(name)
}

// deliverSecret writes the value over the host agent's upload path, which
// carries the bytes on stdin rather than in a command line. a file target
// is created empty and root-only first, because the upload wire carries no
// mode and `cat >` keeps the mode of a file that already exists.
func (fm *FleetManager) deliverSecret(ctx context.Context, envID string, t hitl.SecretTarget, value string) error {
	env, err := fm.guestEnvironment(ctx, envID)
	if err != nil {
		return err
	}
	if t.Env != "" {
		return env.Upload(ctx, []byte(value), hitlEnvDir+"/"+t.Env)
	}
	res, err := env.Exec(ctx, []string{"sh", "-c", secretFilePrep, "fuse-hitl-secret", t.File}, ExecOptions{Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return errors.New("prepare " + t.File + " failed: " + strings.TrimSpace(string(res.Stderr)))
	}
	return env.Upload(ctx, []byte(value), t.File)
}

// secretFilePrep creates a file target empty with mode 0600.
const secretFilePrep = `umask 077 && mkdir -p "$(dirname "$1")" && : > "$1" && chmod 0600 "$1"`

// withSecretEnv wraps an agent's argv so it sees the env secrets a human
// delivered. environments that declare no env targets run argv unchanged.
func (fm *FleetManager) withSecretEnv(envID string, cmd []string) []string {
	fm.mu.RLock()
	v, ok := fm.vms[envID]
	declares := false
	if ok && v.spec.Hitl != nil && v.spec.Hitl.Requests.Secret != nil {
		for _, t := range v.spec.Hitl.Requests.Secret.Targets {
			if t.Env != "" {
				declares = true
				break
			}
		}
	}
	fm.mu.RUnlock()
	if !declares {
		return cmd
	}
	return append([]string{"sh", "-c", hitlEnvWrapper, "fuse-hitl-env"}, cmd...)
}
