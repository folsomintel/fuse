package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// testDoctor returns a doctor on a healthy desktop: the display answers
// 1024x768, the wm is recorded, vnc dials, and memory is half free.
func testDoctor(t *testing.T) *doctor {
	t.Helper()
	comp, _ := testComputer(t)
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemTotal:       2048000 kB\nMemFree:  100 kB\nMemAvailable:   1024000 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &doctor{
		vmID:         "vm-1",
		comp:         comp,
		desktopPath:  filepath.Join(dir, "desktop.json"),
		meminfoPath:  meminfo,
		rootPath:     dir,
		desktopImage: func() bool { return true },
		liveWM:       func() string { return "xfwm4" },
		dial:         func(string) error { return nil },
		diskUsage:    func(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil },
	}
}

func checksByName(r doctorReport) map[string]doctorCheck {
	out := map[string]doctorCheck{}
	for _, c := range r.Checks {
		out[c.Name] = c
	}
	return out
}

func TestDoctorHealthyDesktop(t *testing.T) {
	r := testDoctor(t).report(context.Background())
	if r.Status != checkOK {
		t.Fatalf("status = %s, want ok: %+v", r.Status, r.Checks)
	}
	got := checksByName(r)
	for _, name := range []string{"agent", "display", "wm", "vnc", "healthcheck", "disk", "memory"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("missing check %q in %+v", name, r.Checks)
		}
	}
	if got["display"].Message != ":1 1024x768" {
		t.Fatalf("display message = %q", got["display"].Message)
	}
	if got["healthcheck"].Status != checkSkip {
		t.Fatalf("healthcheck with no probe = %s, want skip", got["healthcheck"].Status)
	}
}

func TestDoctorSkipsDisplayOnHeadlessImage(t *testing.T) {
	d := testDoctor(t)
	d.desktopImage = func() bool { return false }
	r := d.report(context.Background())
	if r.Status != checkOK {
		t.Fatalf("headless image must not fail doctor: %+v", r.Checks)
	}
	for _, name := range []string{"display", "wm", "vnc"} {
		if s := checksByName(r)[name].Status; s != checkSkip {
			t.Fatalf("%s = %s, want skip", name, s)
		}
	}
}

func TestDoctorDisplayDown(t *testing.T) {
	d := testDoctor(t)
	d.comp.ready = func() error { return errors.New("display :1 is not up") }
	r := d.report(context.Background())
	got := checksByName(r)
	if r.Status != checkFail || got["display"].Status != checkFail {
		t.Fatalf("dead display should fail: %+v", r.Checks)
	}
	if got["vnc"].Status != checkSkip || got["wm"].Status != checkSkip {
		t.Fatalf("wm/vnc cannot be judged without a display: %+v", r.Checks)
	}
}

func TestDoctorGeometryAndWMDrift(t *testing.T) {
	d := testDoctor(t)
	if err := os.WriteFile(d.desktopPath, []byte(`{"width":1280,"height":800,"wm":"openbox"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := d.report(context.Background())
	got := checksByName(r)
	if r.Status != checkWarn {
		t.Fatalf("drift is a warn, got %s: %+v", r.Status, r.Checks)
	}
	if got["display"].Message != ":1 1024x768, desktop declares 1280x800" {
		t.Fatalf("display message = %q", got["display"].Message)
	}
	if got["wm"].Message != "xfwm4 running, desktop declares openbox" {
		t.Fatalf("wm message = %q", got["wm"].Message)
	}
}

func TestDoctorVNCDown(t *testing.T) {
	d := testDoctor(t)
	d.dial = func(string) error { return errors.New("connection refused") }
	if s := checksByName(d.report(context.Background()))["vnc"].Status; s != checkFail {
		t.Fatalf("vnc = %s, want fail", s)
	}
}

func TestDoctorHealthcheckVerdicts(t *testing.T) {
	for state, want := range map[string]string{
		healthPassing:  checkOK,
		healthStarting: checkWarn,
		healthFailing:  checkFail,
	} {
		d := testDoctor(t)
		d.probe = &prober{status: healthStatus{State: state, Failures: 3, Message: "refused"}}
		r := d.report(context.Background())
		if s := checksByName(r)["healthcheck"].Status; s != want {
			t.Fatalf("probe %s -> %s, want %s", state, s, want)
		}
		if r.Status != want {
			t.Fatalf("probe %s -> overall %s, want %s", state, r.Status, want)
		}
	}
}

func TestDoctorLowMemory(t *testing.T) {
	d := testDoctor(t)
	if err := os.WriteFile(d.meminfoPath, []byte("MemTotal: 1000000 kB\nMemAvailable: 10000 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := checksByName(d.report(context.Background()))["memory"].Status; s != checkFail {
		t.Fatalf("1%% available = %s, want fail", s)
	}
}

func TestDoctorDiskFull(t *testing.T) {
	d := testDoctor(t)
	d.diskUsage = func(string) (uint64, uint64, error) { return 1 << 20, 100 << 30, nil }
	if s := checksByName(d.report(context.Background()))["disk"].Status; s != checkFail {
		t.Fatalf("near-full disk = %s, want fail", s)
	}
}

func TestStatfsUsage(t *testing.T) {
	free, total, err := statfsUsage(t.TempDir())
	if err != nil || total == 0 || free > total {
		t.Fatalf("statfsUsage = %d/%d, %v", free, total, err)
	}
}

func TestFreeStatus(t *testing.T) {
	cases := []struct {
		free, total uint64
		want        string
	}{
		{50, 100, checkOK},
		{5, 100, checkWarn},
		{1, 100, checkFail},
		{0, 0, checkWarn},
	}
	for _, c := range cases {
		if got := freeStatus(c.free, c.total); got != c.want {
			t.Fatalf("freeStatus(%d, %d) = %s, want %s", c.free, c.total, got, c.want)
		}
	}
}

func TestDoctorRouteIsProtected(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, newCredentials("secret-token"), nil, 0, false, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/doctor")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated doctor = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/doctor", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("doctor = %d, want 200", resp.StatusCode)
	}
	var r doctorReport
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.VMID != "fuse-1" || len(r.Checks) == 0 || r.Status == "" {
		t.Fatalf("report = %+v", r)
	}
}
