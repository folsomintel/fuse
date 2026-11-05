package fuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Hitl request and signal kinds added for secrets, takeover and signals.
const (
	HitlKindSecret   = "secret"
	HitlKindTakeover = "takeover"
	HitlKindStall    = "stall"
	HitlKindExpiry   = "expiry"
)

// TakeoverViaTerminal is the only way in so far: the human gets their own
// shell on the attach path.
const TakeoverViaTerminal = "terminal"

// TakeoverViaDesktop gives the human the live desktop view, recorded as an
// input-action log plus periodic screenshots. It needs a desktop block.
const TakeoverViaDesktop = "desktop"

// SecretAnswer is what an agent's secret request resolves to. The value
// itself never comes back.
const SecretAnswer = "done"

// hitl audit events for secrets and takeover.
const (
	AuditSecretAnswered  = "secret_answered"
	AuditTakeoverStarted = "takeover_started"
	AuditTakeoverEnded   = "takeover_ended"
	// AuditTakeoverSession closes one recorded shell; its RecordingID
	// names the cast.
	AuditTakeoverSession = "takeover_session"
)

// CodeLocked is the api error code of a 423: a human holds the environment.
const CodeLocked = "locked"

// HitlSecretRequests declares where a secret may land. The agent names a
// target; it never picks one.
type HitlSecretRequests struct {
	Targets []HitlSecretTarget `json:"targets"`
}

// HitlSecretTarget is one declared destination: exactly one of Env (a
// variable later execs see) or File (an absolute guest path, written 0600).
// Label is what the human is shown first.
type HitlSecretTarget struct {
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
	Label string `json:"label"`
}

// HitlTakeoverRequests allows takeover. Via lists the ways in; empty means
// terminal.
type HitlTakeoverRequests struct {
	Via []string `json:"via,omitempty"`
}

// SecretPayload is a secret request. Label is the Fusefile's; Message is
// the agent's own text.
type SecretPayload struct {
	Target  string `json:"target"`
	Label   string `json:"label"`
	Message string `json:"message,omitempty"`
}

// TakeoverPayload is a takeover. Holder and StartedAt are set while a human
// holds the environment.
type TakeoverPayload struct {
	Via       string     `json:"via"`
	Reason    string     `json:"reason,omitempty"`
	Holder    *Actor     `json:"holder,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// SignalPayload is what fuse noticed on its own: a stall's failure run or
// quiet time, or the reap an expiry warns about.
type SignalPayload struct {
	Failures      int        `json:"failures,omitempty"`
	LastExitCode  int        `json:"last_exit_code,omitempty"`
	QuietSeconds  int64      `json:"quiet_seconds,omitempty"`
	Reaper        string     `json:"reaper,omitempty"`
	ReapAt        *time.Time `json:"reap_at,omitempty"`
	ExtendedUntil *time.Time `json:"extended_until,omitempty"`
}

// LockedError is an exec or computer call refused with 423 because a human
// holds the environment, returned when waiting is off. Wait on
// InterventionID with Interventions.Wait, then call again.
type LockedError struct {
	InterventionID string
	// Kind is HitlKindTakeover or HitlKindStall.
	Kind string
	Err  *APIError
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("environment locked by %s intervention %s", e.Kind, e.InterventionID)
}

func (e *LockedError) Unwrap() error { return e.Err }

// IsLocked reports whether err is a call refused because a human holds the
// environment.
func IsLocked(err error) bool {
	var l *LockedError
	if errors.As(err, &l) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusLocked
}

// lockedFrom returns the lock behind a 423, or nil for any other error.
func lockedFrom(err error) *LockedError {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusLocked {
		return nil
	}
	return &LockedError{InterventionID: apiErr.Details["intervention_id"], Kind: apiErr.Details["kind"], Err: apiErr}
}

// retryLocked runs call, and while the environment is locked waits for the
// lock's intervention to resolve and runs it again. every lock has a
// deadline, so this ends; ctx bounds it too.
func retryLocked[T any](ctx context.Context, t *transport, noWait bool, call func() (T, error)) (T, error) {
	for {
		res, err := call()
		l := lockedFrom(err)
		if l == nil {
			return res, err
		}
		var zero T
		if noWait || (t != nil && t.hitlNoWait) || l.InterventionID == "" {
			return zero, l
		}
		if _, err := waitIntervention(ctx, t, l.InterventionID); err != nil {
			return zero, err
		}
	}
}

// AnswerSecret answers a secret request with its value. Fuse writes the
// value into the declared guest target and drops it; it is never stored or
// returned, and the agent gets back SecretAnswer. Needs the master token
// or a key with hitl:secret.
func (s *InterventionsService) AnswerSecret(ctx context.Context, id, value string) (*Intervention, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("interventions service is not configured")
	}
	if id == "" {
		return nil, errors.New("intervention id is required")
	}
	if value == "" {
		return nil, errors.New("value is required")
	}
	values := url.Values{}
	values.Set("action", "secret")
	var in Intervention
	body := struct {
		Value string `json:"value"`
	}{value}
	if err := doJSON(ctx, s.t, http.MethodPost, "/v1/interventions/"+url.PathEscape(id), values, body, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// Recording returns one recorded takeover session, decrypted by the
// orchestrator: an asciinema v2 cast for a terminal session, json (an
// input-action log plus screenshots) for a desktop one.
// takeover_session audit rows name it.
// Needs hitl:read.
func (s *InterventionsService) Recording(ctx context.Context, recordingID string) ([]byte, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("interventions service is not configured")
	}
	if recordingID == "" {
		return nil, errors.New("recording id is required")
	}
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/hitl/recordings/"+url.PathEscape(recordingID), nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.doStream(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// TakeoverOptions starts a takeover.
type TakeoverOptions struct {
	// Via is TakeoverViaTerminal (the default) or TakeoverViaDesktop.
	Via string `json:"via,omitempty"`
	// Reason is recorded when there is no agent request to claim.
	Reason string `json:"reason,omitempty"`
}

// Takeover gives the caller control of an environment. It claims the
// agent's pending takeover request, or opens one. Until Release (or the
// deadline), the agent's exec and computer calls wait. Open the shell
// with TakeoverAttach. Needs the master token or a key with hitl:takeover.
func (s *EnvironmentsService) Takeover(ctx context.Context, vmID string, opts TakeoverOptions) (*Intervention, error) {
	return s.takeoverAction(ctx, vmID, "takeover", opts)
}

// Release ends the held takeover and hands the note back to the agent as
// the takeover's answer.
func (s *EnvironmentsService) Release(ctx context.Context, vmID, note string) (*Intervention, error) {
	return s.takeoverAction(ctx, vmID, "release", struct {
		Note string `json:"note,omitempty"`
	}{note})
}

func (s *EnvironmentsService) takeoverAction(ctx context.Context, vmID, action string, body any) (*Intervention, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	values := url.Values{}
	values.Set("action", action)
	var in Intervention
	if err := doJSON(ctx, s.t, http.MethodPost, "/v1/environments/"+url.PathEscape(vmID), values, body, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// TakeoverAttach opens the takeover holder's shell. It is Attach on the
// takeover's own route: open to a hitl:takeover key, only for the holder,
// and recorded as an encrypted cast with a banner saying so.
func (s *EnvironmentsService) TakeoverAttach(ctx context.Context, interventionID string, opts AttachOptions) (*AttachStream, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if interventionID == "" {
		return nil, errors.New("intervention id is required")
	}
	q := url.Values{}
	q.Set("tty", "1")
	if opts.Rows > 0 {
		q.Set("rows", strconv.Itoa(int(opts.Rows)))
	}
	if opts.Cols > 0 {
		q.Set("cols", strconv.Itoa(int(opts.Cols)))
	}
	for _, arg := range opts.Cmd {
		q.Add("cmd", arg)
	}
	path := "/v1/interventions/" + url.PathEscape(interventionID) + "/attach"
	conn, br, err := s.t.dialUpgrade(ctx, path, q, AttachProto, "takeover attach")
	if err != nil {
		return nil, err
	}
	return &AttachStream{conn: conn, r: br}, nil
}

// TakeoverDesktop opens the takeover holder's live desktop: the computer
// stream on the takeover's own route, recorded as an input-action log plus
// periodic screenshots. The plain ComputerStream answers 423 while the
// takeover is held. The takeover must have been started with
// TakeoverViaDesktop.
func (s *EnvironmentsService) TakeoverDesktop(ctx context.Context, interventionID string) (*ComputerStream, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if interventionID == "" {
		return nil, errors.New("intervention id is required")
	}
	path := "/v1/interventions/" + url.PathEscape(interventionID) + "/desktop"
	conn, br, err := s.t.dialUpgrade(ctx, path, url.Values{}, VNCProto, "takeover desktop")
	if err != nil {
		return nil, err
	}
	return &ComputerStream{conn: conn, r: br}, nil
}
