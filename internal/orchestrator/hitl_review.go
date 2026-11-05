package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// reviewReadTimeout bounds reading a review's file from the guest.
const reviewReadTimeout = 30 * time.Second

// readReviewFile reads a guest file for a review request, at most
// hitl.MaxReviewBytes of it, so the human reviews what the agent submitted
// even if the file changes afterwards. it goes to the guest directly, not
// through Exec: this is fuse reading on the agent's behalf, not an agent
// exec, so the takeover lock and the stall counters stay out of it.
func (fm *FleetManager) readReviewFile(ctx context.Context, envID, path string) (string, bool, error) {
	env, err := fm.guestEnvironment(ctx, envID)
	if err != nil {
		return "", false, err
	}
	// one byte past the cap tells a long file from one exactly at it.
	cmd := []string{"head", "-c", strconv.Itoa(hitl.MaxReviewBytes + 1), "--", path}
	res, err := env.Exec(ctx, cmd, ExecOptions{Timeout: reviewReadTimeout})
	if err != nil {
		return "", false, fmt.Errorf("read review file in vm %s: %w", envID, err)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = fmt.Sprintf("head exited %d", res.ExitCode)
		}
		return "", false, fmt.Errorf("%w: read %s: %s", ErrInvalidIntervention, path, msg)
	}
	out := res.Stdout
	truncated := len(out) > hitl.MaxReviewBytes
	if truncated {
		out = out[:hitl.MaxReviewBytes]
	}
	return string(out), truncated, nil
}
