package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// deciderClient posts to gate deciders. a redirect would send the signed
// action somewhere the author did not name, so it counts as a failure, and
// a failure is ask.
var deciderClient = &http.Client{
	Timeout:       hitl.DeciderTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// askDecider posts a held action to its gate's decider and returns the
// answer. any error means the caller falls through to a human. the whole
// call, screenshot included, is bounded by hitl.DeciderTimeout plus the
// screenshot's own bound, so a slow decider costs the agent seconds, not
// the gate's full deadline.
func (fm *FleetManager) askDecider(ctx context.Context, g hitl.Gate, in hitl.Intervention) (hitl.DeciderResponse, error) {
	fm.mu.RLock()
	v := fm.vms[in.EnvID]
	var secret string
	ok := false
	if v != nil {
		secret, ok = v.hitlKeys[g.Decide.Secret]
	}
	fm.mu.RUnlock()
	if !ok {
		return hitl.DeciderResponse{}, fmt.Errorf("signing secret %q is unavailable", g.Decide.Secret)
	}

	req := hitl.DeciderRequest{
		EnvID: in.EnvID, Gate: g.Name, On: in.Kind, TriggeredBy: in.TriggeredBy,
		Exec: in.Exec, Lifecycle: in.Lifecycle, Computer: in.Computer, Deadline: in.Deadline,
	}
	if in.Computer != nil {
		req.Screenshot = fm.deciderScreenshot(ctx, in.EnvID)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return hitl.DeciderResponse{}, err
	}

	cctx, cancel := context.WithTimeout(ctx, hitl.DeciderTimeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, g.Decide.URL, bytes.NewReader(body))
	if err != nil {
		return hitl.DeciderResponse{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("User-Agent", "fuse-orchestrator")
	hr.Header.Set(hitl.HeaderSignature, hitl.Sign([]byte(secret), time.Now(), body))
	resp, err := deciderClient.Do(hr)
	if err != nil {
		return hitl.DeciderResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return hitl.DeciderResponse{}, fmt.Errorf("decider answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	if err != nil {
		return hitl.DeciderResponse{}, err
	}
	return hitl.ParseDeciderResponse(raw)
}

// deciderScreenshotTimeout bounds the screenshot taken for a decider.
const deciderScreenshotTimeout = 5 * time.Second

// deciderScreenshot takes a screenshot of the display for a decider to
// look at. it returns "" when none could be taken in time; the decider is
// still asked, with what it has.
func (fm *FleetManager) deciderScreenshot(ctx context.Context, envID string) string {
	ctx, cancel := context.WithTimeout(ctx, deciderScreenshotTimeout)
	defer cancel()
	res, err := fm.guestComputer(ctx, envID, http.MethodGet, "/v1/computer/screenshot", nil)
	if err != nil || res.Status != http.StatusOK {
		return ""
	}
	var shot hitl.ComputerResult
	if json.Unmarshal(res.Body, &shot) != nil {
		return ""
	}
	return shot.Screenshot
}
