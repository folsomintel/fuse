package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Hitl request kinds.
const (
	HitlKindAsk = "ask"
)

// Intervention states. pending is the only non-terminal one.
const (
	InterventionPending   = "pending"
	InterventionApproved  = "approved"
	InterventionRejected  = "rejected"
	InterventionExpired   = "expired"
	InterventionCancelled = "cancelled"
)

// Intervention outcomes. every terminal intervention carries one.
const (
	OutcomeApprove = "approve"
	OutcomeReject  = "reject"
)

// HitlSpec is the human-in-the-loop policy an environment is created with.
type HitlSpec struct {
	// TimeoutSeconds is the default deadline for every intervention, 10 to
	// 86400. Zero means 600.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	// OnTimeout is "reject" (the default) or "allow".
	OnTimeout string `json:"on_timeout,omitempty"`
	// Requests declares the request kinds the agent may raise.
	Requests HitlRequests `json:"requests"`
	// RequireActor refuses decisions that do not carry a pinned or
	// asserted actor.
	RequireActor bool `json:"require_actor,omitempty"`
	// Notify are webhooks told about every intervention and audit event.
	Notify []HitlWebhook `json:"notify,omitempty"`
}

// HitlWebhook is one hitl.notify target. Secret names an entry in the
// create request's secrets map, used to sign deliveries.
type HitlWebhook struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

// HitlRequests declares the allowed request kinds. Only Ask exists today;
// set it to a non-nil value to allow asks.
type HitlRequests struct {
	Ask *HitlAskRequests `json:"ask,omitempty"`
}

// HitlAskRequests configures ask requests. Present means allowed.
type HitlAskRequests struct{}

// Actor is who raised or decided an intervention: "master", "key:<id>",
// or "system" for fuse itself (expiry, cancel on destroy).
type Actor struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Source string `json:"source"`
	KeyID  string `json:"key_id,omitempty"`
}

// InterventionPayload is what the agent asked.
type InterventionPayload struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
}

// InterventionDecision is how an intervention resolved. An expired one
// carries the on_timeout outcome and a cancelled one a reject.
type InterventionDecision struct {
	Outcome   string    `json:"outcome"`
	Answer    string    `json:"answer,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	DecidedBy Actor     `json:"decided_by"`
	DecidedAt time.Time `json:"decided_at"`
}

// Intervention is one request for a human.
type Intervention struct {
	ID          string                `json:"id"`
	EnvID       string                `json:"env_id"`
	Kind        string                `json:"kind"`
	State       string                `json:"state"`
	Payload     InterventionPayload   `json:"payload"`
	TriggeredBy Actor                 `json:"triggered_by"`
	Deadline    time.Time             `json:"deadline"`
	OnTimeout   string                `json:"on_timeout"`
	Decision    *InterventionDecision `json:"decision,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
}

// Terminal reports whether the intervention is resolved.
func (in *Intervention) Terminal() bool {
	return in != nil && in.State != "" && in.State != InterventionPending
}

// Approved reports whether the intervention resolved as approve, which
// includes an expiry under on_timeout: allow.
func (in *Intervention) Approved() bool {
	return in != nil && in.Decision != nil && in.Decision.Outcome == OutcomeApprove
}

// HumanRequest is what RequestHuman sends.
type HumanRequest struct {
	// Kind is the request kind. Empty means HitlKindAsk.
	Kind     string   `json:"kind,omitempty"`
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
	// TimeoutSeconds shortens the policy's deadline. It can never extend it.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	// NoWait returns the pending intervention at once instead of waiting
	// for a decision. Wait on it later with Interventions.Wait.
	NoWait bool `json:"-"`
}

// RequestHuman asks a human for something the environment's hitl policy
// declares. By default it blocks until the intervention resolves (answered,
// rejected, expired or cancelled) or ctx is done; read the result off the
// returned intervention's Decision. Set NoWait to get the pending handle.
func (s *EnvironmentsService) RequestHuman(ctx context.Context, vmID string, r HumanRequest) (*Intervention, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	if r.Kind == "" {
		r.Kind = HitlKindAsk
	}
	values := url.Values{}
	values.Set("action", "request")
	var in Intervention
	if err := doJSON(ctx, s.t, http.MethodPost, "/v1/environments/"+url.PathEscape(vmID), values, r, &in); err != nil {
		return nil, err
	}
	if r.NoWait {
		return &in, nil
	}
	return waitIntervention(ctx, s.t, in.ID)
}

// InterventionsService reads and decides hitl interventions.
type InterventionsService struct {
	t *transport
}

func newInterventionsService(t *transport) *InterventionsService {
	return &InterventionsService{t: t}
}

// ListInterventionsOptions filters a list. Empty fields match everything.
type ListInterventionsOptions struct {
	EnvID string
	State string
	// Actor matches interventions raised or decided by this actor id.
	Actor  string
	Limit  int
	Cursor string
}

// InterventionPage is one page of a ListPage call.
type InterventionPage struct {
	Interventions []Intervention `json:"interventions"`
	// NextCursor is empty once there are no more results.
	NextCursor string `json:"next_cursor,omitempty"`
}

// List returns every intervention matching opt, oldest first, walking every
// page.
func (s *InterventionsService) List(ctx context.Context, opt ListInterventionsOptions) ([]Intervention, error) {
	var out []Intervention
	opt.Limit = maxPageLimit
	for {
		page, err := s.ListPage(ctx, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Interventions...)
		if page.NextCursor == "" {
			return out, nil
		}
		opt.Cursor = page.NextCursor
	}
}

// ListPage returns one page of interventions matching opt.
func (s *InterventionsService) ListPage(ctx context.Context, opt ListInterventionsOptions) (*InterventionPage, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("interventions service is not configured")
	}
	values := url.Values{}
	if opt.EnvID != "" {
		values.Set("env", opt.EnvID)
	}
	if opt.State != "" {
		values.Set("state", opt.State)
	}
	if opt.Actor != "" {
		values.Set("actor", opt.Actor)
	}
	setPaginationParams(values, opt.Limit, opt.Cursor)
	var out struct {
		Interventions []Intervention `json:"interventions"`
		NextCursor    *string        `json:"next_cursor,omitempty"`
	}
	if err := doJSON(ctx, s.t, http.MethodGet, "/v1/interventions", values, nil, &out); err != nil {
		return nil, err
	}
	page := &InterventionPage{Interventions: out.Interventions}
	if out.NextCursor != nil {
		page.NextCursor = *out.NextCursor
	}
	return page, nil
}

// Get returns one intervention without waiting.
func (s *InterventionsService) Get(ctx context.Context, id string) (*Intervention, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("interventions service is not configured")
	}
	return getIntervention(ctx, s.t, id, 0)
}

// Wait blocks until the intervention resolves or ctx is done. Every
// intervention has a deadline, so this always returns eventually. A
// dropped connection or an orchestrator restart is survived by polling
// again; postgres, not the connection, holds the state.
func (s *InterventionsService) Wait(ctx context.Context, id string) (*Intervention, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("interventions service is not configured")
	}
	return waitIntervention(ctx, s.t, id)
}

// Approve resolves a pending intervention as approved. An ask needs an
// answer (one of its choices, when it has any). Needs the master token or
// a key with hitl:decide, and not the key or actor that raised it.
func (s *InterventionsService) Approve(ctx context.Context, id, answer, reason string) (*Intervention, error) {
	return s.decide(ctx, id, "approve", answer, reason)
}

// Reject resolves a pending intervention as rejected. The reason is fed
// back to the agent; one is filled in when empty. Needs the master token
// or a key with hitl:decide, and not the key or actor that raised it.
func (s *InterventionsService) Reject(ctx context.Context, id, reason string) (*Intervention, error) {
	return s.decide(ctx, id, "reject", "", reason)
}

func (s *InterventionsService) decide(ctx context.Context, id, action, answer, reason string) (*Intervention, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("interventions service is not configured")
	}
	if id == "" {
		return nil, errors.New("intervention id is required")
	}
	values := url.Values{}
	values.Set("action", action)
	body := struct {
		Answer string `json:"answer,omitempty"`
		Reason string `json:"reason,omitempty"`
	}{answer, reason}
	var in Intervention
	if err := doJSON(ctx, s.t, http.MethodPost, "/v1/interventions/"+url.PathEscape(id), values, body, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// hitl audit events.
const (
	AuditRaised       = "raised"
	AuditDecided      = "decided"
	AuditExpired      = "expired"
	AuditCancelled    = "cancelled"
	AuditInviteMinted = "invite_minted"
	AuditInviteUsed   = "invite_used"
)

// AuditOptions filters an audit read. Empty fields match everything.
type AuditOptions struct {
	EnvID string
	Actor string
	// Since is an RFC 3339 time or a duration back from now, like "24h".
	Since  string
	Limit  int
	Cursor string
}

// AuditEntry is one row of the hash-chained hitl audit log. It records who
// did what and from where, never content.
type AuditEntry struct {
	Seq            int64     `json:"seq"`
	Event          string    `json:"event"`
	InterventionID string    `json:"intervention_id,omitempty"`
	EnvID          string    `json:"env_id,omitempty"`
	Actor          Actor     `json:"actor"`
	RemoteAddr     string    `json:"remote_addr,omitempty"`
	At             time.Time `json:"at"`
	// PrevHash and Hash are hex; each row's PrevHash is the Hash before it.
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// AuditPage is one page of an Audit call.
type AuditPage struct {
	Entries []AuditEntry `json:"entries"`
	// NextCursor is empty once there are no more results.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Audit returns one page of the hitl audit log, oldest first. A key scoped
// to an environment sees only that environment's rows. Needs hitl:read.
func (s *InterventionsService) Audit(ctx context.Context, opt AuditOptions) (*AuditPage, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("interventions service is not configured")
	}
	values := url.Values{}
	if opt.EnvID != "" {
		values.Set("env", opt.EnvID)
	}
	if opt.Actor != "" {
		values.Set("actor", opt.Actor)
	}
	if opt.Since != "" {
		values.Set("since", opt.Since)
	}
	setPaginationParams(values, opt.Limit, opt.Cursor)
	var out struct {
		Entries    []AuditEntry `json:"entries"`
		NextCursor *string      `json:"next_cursor,omitempty"`
	}
	if err := doJSON(ctx, s.t, http.MethodGet, "/v1/hitl/audit", values, nil, &out); err != nil {
		return nil, err
	}
	page := &AuditPage{Entries: out.Entries}
	if out.NextCursor != nil {
		page.NextCursor = *out.NextCursor
	}
	return page, nil
}

// AuditAll returns every audit entry matching opt, oldest first, walking
// every page.
func (s *InterventionsService) AuditAll(ctx context.Context, opt AuditOptions) ([]AuditEntry, error) {
	var out []AuditEntry
	opt.Limit = maxPageLimit
	for {
		page, err := s.Audit(ctx, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Entries...)
		if page.NextCursor == "" {
			return out, nil
		}
		opt.Cursor = page.NextCursor
	}
}

// waitPoll is how long one long-poll asks the server to hold. it stays well
// under the client's default 60s timeout.
const waitPoll = 30 * time.Second

func waitIntervention(ctx context.Context, t *transport, id string) (*Intervention, error) {
	for {
		in, err := getIntervention(ctx, t, id, waitPoll)
		if err != nil {
			return nil, err
		}
		if in.Terminal() {
			return in, nil
		}
		if err := ctx.Err(); err != nil {
			return in, err
		}
	}
}

func getIntervention(ctx context.Context, t *transport, id string, wait time.Duration) (*Intervention, error) {
	if id == "" {
		return nil, errors.New("intervention id is required")
	}
	var values url.Values
	if wait > 0 {
		values = url.Values{}
		values.Set("wait", fmt.Sprintf("%ds", int(wait.Seconds())))
	}
	var in Intervention
	if err := doJSON(ctx, t, http.MethodGet, "/v1/interventions/"+url.PathEscape(id), values, nil, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// doJSON sends one request and decodes a json response into out.
func doJSON(ctx context.Context, t *transport, method, path string, query url.Values, body, out any) error {
	req, err := t.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	resp, err := t.do(req)
	if err != nil {
		return err
	}
	if err := CheckResponse(resp); err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
