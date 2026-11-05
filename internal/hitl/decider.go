package hitl

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// a decider is the extension point in front of the human: before an ask
// gate holds an action for a person, fuse posts the action to the gate's
// decide url, signed like a webhook, and the endpoint answers allow, deny
// or ask. classifiers, llm judges and business rules live there, so fuse
// never grows a policy language of its own. any failure (timeout, transport
// error, non-2xx, a body that does not parse) falls through to ask: a
// broken decider can only ever send more work to a human, never less.

// DeciderTimeout bounds one decider call. it is fixed, not configurable,
// because the agent's call is blocked on it.
const DeciderTimeout = 5 * time.Second

// maxDeciderResponse bounds the answer read back.
const maxDeciderResponse = 64 << 10

// ActorDecider marks a decision a decider made.
const ActorDecider ActorSource = "decider"

// Decider is a gate's decide webhook. Secret names an environment secret
// whose value signs each call (the Fuse-Signature header, as for notify).
type Decider struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

func (d Decider) validate(at string) []error {
	var errs []error
	if err := ValidateWebhookURL(d.URL); err != nil {
		errs = append(errs, fmt.Errorf("%s.url: %w", at, err))
	}
	if d.Secret == "" {
		errs = append(errs, fmt.Errorf("%s.secret: name the secret that signs decider calls", at))
	}
	return errs
}

// DeciderActor is who a decider's decision is recorded as.
func DeciderActor(gate string) Actor {
	return Actor{ID: "decider:" + gate, Source: ActorDecider}
}

// DeciderVerdict is a decider's answer.
type DeciderVerdict string

const (
	DeciderAllow DeciderVerdict = "allow"
	DeciderDeny  DeciderVerdict = "deny"
	DeciderAsk   DeciderVerdict = "ask"
)

// DeciderRequest is the body fuse posts to a decider. it carries the
// action itself, unlike a notify delivery: judging it is the point.
type DeciderRequest struct {
	EnvID       string            `json:"env_id"`
	Gate        string            `json:"gate"`
	On          Kind              `json:"on"`
	TriggeredBy Actor             `json:"triggered_by"`
	Exec        *ExecPayload      `json:"exec,omitempty"`
	Lifecycle   *DeciderLifecycle `json:"lifecycle,omitempty"`
	Computer    *ComputerPayload  `json:"computer,omitempty"`
	// Screenshot is a base64 png of the display, for a computer action. it
	// is empty when one could not be taken in time.
	Screenshot string    `json:"screenshot,omitempty"`
	Deadline   time.Time `json:"deadline"`
}

// DeciderLifecycle is a held lifecycle action as a decider sees it: the
// params as json, not the base64 a []byte would marshal to.
type DeciderLifecycle struct {
	Action LifecycleAction `json:"action"`
	Params json.RawMessage `json:"params,omitempty"`
}

// NewDeciderLifecycle converts a held lifecycle payload for a decider.
func NewDeciderLifecycle(p *LifecyclePayload) *DeciderLifecycle {
	if p == nil {
		return nil
	}
	return &DeciderLifecycle{Action: p.Action, Params: json.RawMessage(p.Params)}
}

// DeciderResponse is what a decider answers.
type DeciderResponse struct {
	Decision DeciderVerdict `json:"decision"`
	// Reason goes back to the agent on a deny, and to the human on an ask.
	Reason string `json:"reason,omitempty"`
}

// DeciderNote records what the decider said about an action it sent on to
// a human: it escalated (ask), or it failed and fuse fell through.
type DeciderNote struct {
	Verdict DeciderVerdict `json:"verdict"`
	Reason  string         `json:"reason,omitempty"`
	// Error is set when the call failed and fuse fell through to ask.
	Error string `json:"error,omitempty"`
}

// ParseDeciderResponse reads a decider's answer. an unknown decision is an
// error, which the caller turns into ask.
func ParseDeciderResponse(body []byte) (DeciderResponse, error) {
	var r DeciderResponse
	if len(body) > maxDeciderResponse {
		return r, fmt.Errorf("decider answer is over %d bytes", maxDeciderResponse)
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return r, fmt.Errorf("decider answer is not json: %w", err)
	}
	r.Decision = DeciderVerdict(strings.ToLower(strings.TrimSpace(string(r.Decision))))
	switch r.Decision {
	case DeciderAllow, DeciderDeny, DeciderAsk:
	default:
		return r, fmt.Errorf("decider answered %q; want allow, deny or ask", r.Decision)
	}
	if len(r.Reason) > maxReasonLen {
		r.Reason = r.Reason[:maxReasonLen]
	}
	return r, nil
}
