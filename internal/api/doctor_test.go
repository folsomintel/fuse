package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEnvironmentDoctorProxiesToGuest(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)

	guest := newFakeGuest(http.StatusOK, `{"status":"warn","version":"v1","checks":[{"name":"display","status":"warn","message":":1 1024x768, desktop declares 1280x800"}]}`)
	defer guest.close()
	env.url = guest.authority()

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-task-1/doctor", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if guest.gotPath != "/v1/doctor" {
		t.Fatalf("guest saw %s, want /v1/doctor", guest.gotPath)
	}
	var rep DoctorReport
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != "warn" || len(rep.Checks) != 1 || rep.Checks[0].Name != "display" {
		t.Fatalf("report not relayed: %+v", rep)
	}
}

func TestEnvironmentDoctorOldGuestIs501(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)

	guest := newFakeGuest(http.StatusNotFound, "404 page not found\n")
	defer guest.close()
	env.url = guest.authority()

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-task-1/doctor", nil)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 body=%s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); !strings.Contains(e.Error.Message, "predates doctor") {
		t.Fatalf("message = %q", e.Error.Message)
	}
}
