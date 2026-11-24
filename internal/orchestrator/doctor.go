package orchestrator

import (
	"context"
	"net/http"
	"time"
)

// guestDoctorTimeout bounds one doctor relay. fused bounds its own report at
// 10s; a guest that cannot answer within this is the sick guest the caller
// is asking about, and they should hear so quickly rather than after the
// 150s computer-action budget.
const guestDoctorTimeout = 20 * time.Second

// GuestDoctor relays the guest agent's self-check (fused's GET /v1/doctor):
// identity, dbus, disk, memory, each display piece, the healthcheck, dns,
// egress and the clock, each graded pass/warn/fail/skip. Like ComputerDisplay the body passes through
// verbatim, since the check list is owned by the guest agent and grows with
// the image rather than with the control plane.
//
// It is not activity: an operator polling doctor must not hold off the idle
// reaper.
func (fm *FleetManager) GuestDoctor(ctx context.Context, vmID string) (GuestHTTPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, guestDoctorTimeout)
	defer cancel()
	return fm.guestHTTP(ctx, vmID, http.MethodGet, "/v1/doctor", nil, false)
}
