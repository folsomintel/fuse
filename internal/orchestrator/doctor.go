package orchestrator

import (
	"context"
	"net/http"
)

// GuestDoctor relays the guest agent's self-check (fused's GET /v1/doctor):
// agent, display, window manager, vnc, healthcheck, disk and memory, each
// graded ok/warn/fail/skip. Like ComputerDisplay the body passes through
// verbatim, since the check list is owned by the guest agent and grows with
// the image rather than with the control plane.
func (fm *FleetManager) GuestDoctor(ctx context.Context, vmID string) (GuestHTTPResponse, error) {
	return fm.guestComputer(ctx, vmID, http.MethodGet, "/v1/doctor", nil)
}
