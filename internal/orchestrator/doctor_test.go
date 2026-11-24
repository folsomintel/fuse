package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// doctor is a read: polling it must not hold off the idle reaper, unlike a
// computer action, which is exactly the activity the reaper must respect.
func TestGuestDoctorIsNotActivity(t *testing.T) {
	p := newMockProvider()
	fm := newInlineFleet(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"pass","version":"v1","checks":[]}`))
	}))
	defer srv.Close()
	mockEnvOf(fm, vmID).url = strings.TrimPrefix(srv.URL, "http://")

	stale := time.Now().Add(-time.Hour)
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = stale
	fm.mu.Unlock()

	res, err := fm.GuestDoctor(context.Background(), vmID)
	if err != nil || res.Status != http.StatusOK || gotPath != "/v1/doctor" {
		t.Fatalf("doctor = %d %v, guest saw %q", res.Status, err, gotPath)
	}
	fm.mu.RLock()
	got := fm.vms[vmID].lastActivityAt
	fm.mu.RUnlock()
	if !got.Equal(stale) {
		t.Fatalf("doctor bumped lastActivityAt to %v", got)
	}

	// the contrast: a computer action through the same path does bump it,
	// so the assertion above is not passing because nothing ever touches.
	if _, err := fm.ComputerAction(context.Background(), vmID, []byte(`{"action":"screenshot"}`)); err != nil {
		t.Fatalf("computer action: %v", err)
	}
	fm.mu.RLock()
	got = fm.vms[vmID].lastActivityAt
	fm.mu.RUnlock()
	if !got.After(stale) {
		t.Fatal("computer action should count as activity")
	}
}

// a guest that never answers is reported as ErrGuestTimeout, which the api
// maps to a 503, rather than the bare deadline error that would be a 500.
func TestGuestDoctorHungGuestIsTimeout(t *testing.T) {
	p := newMockProvider()
	fm := newInlineFleet(FleetConfig{Provider: p, Prefix: "fuse-"})
	vmID := provisionRunning(t, fm)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	mockEnvOf(fm, vmID).url = strings.TrimPrefix(srv.URL, "http://")

	old := guestDoctorTimeout
	guestDoctorTimeout = 50 * time.Millisecond
	t.Cleanup(func() { guestDoctorTimeout = old })

	_, err := fm.GuestDoctor(context.Background(), vmID)
	if !errors.Is(err, ErrGuestTimeout) {
		t.Fatalf("err = %v, want ErrGuestTimeout", err)
	}
}
