package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// testDoctor returns a doctor on a healthy desktop with direct egress: every
// display piece present at 1024x768, the wm recorded, vnc dialing, dns and
// egress answering, the clock in step, and disk and memory half free.
func testDoctor(t *testing.T) *doctor {
	t.Helper()
	comp, _ := testComputer(t)
	comp.pieces = func() []displayPiece {
		return []displayPiece{{name: "xdotool"}, {name: "scrot"}, {name: "display"}}
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return &doctor{
		vmID:          "vm-1",
		comp:          comp,
		desktopPath:   filepath.Join(dir, "desktop.json"),
		egressPath:    filepath.Join(dir, "egress.json"),
		meminfoPath:   write("meminfo", "MemTotal:       2048000 kB\nMemFree:  100 kB\nMemAvailable:   1024000 kB\n"),
		machineIDPath: write("machine-id", "0123456789abcdef0123456789abcdef\n"),
		dbusPath:      write("bus", ""),
		rootPath:      dir,
		liveWM:        func() string { return "xfwm4" },
		dial:          func(string) error { return nil },
		diskUsage:     func(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil },
		lookup:        func(context.Context, string) error { return nil },
		fetch: func(context.Context, string, *url.URL) (time.Time, error) {
			return testNow.Add(-time.Second), nil
		},
		now: func() time.Time { return testNow },
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
	if r.Status != checkPass {
		t.Fatalf("status = %s, want pass: %+v", r.Status, r.Checks)
	}
	want := []string{"agent", "machine-id", "dbus", "disk", "memory", "xdotool", "scrot", "display", "wm", "vnc", "healthcheck", "dns", "egress", "clock"}
	if len(r.Checks) != len(want) {
		t.Fatalf("checks = %+v, want %v", r.Checks, want)
	}
	for i, name := range want {
		if r.Checks[i].Name != name {
			t.Fatalf("check %d = %q, want %q", i, r.Checks[i].Name, name)
		}
	}
	got := checksByName(r)
	if got["display"].Message != ":1 1024x768" {
		t.Fatalf("display message = %q", got["display"].Message)
	}
	if got["machine-id"].Message != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("machine-id message = %q", got["machine-id"].Message)
	}
	if got["healthcheck"].Status != checkSkip {
		t.Fatalf("healthcheck with no probe = %s, want skip", got["healthcheck"].Status)
	}
}

func TestDoctorSkipsDesktopOnHeadlessImage(t *testing.T) {
	d := testDoctor(t)
	d.comp.pieces = func() []displayPiece {
		return []displayPiece{
			{name: "xdotool", err: errors.New("xdotool not installed")},
			{name: "scrot", err: errors.New("scrot not installed")},
			{name: "display", err: errors.New("display :1 is not up")},
		}
	}
	r := d.report(context.Background())
	if r.Status != checkPass {
		t.Fatalf("headless image must not fail doctor: %+v", r.Checks)
	}
	for _, name := range []string{"xdotool", "scrot", "display", "wm", "vnc"} {
		if c := checksByName(r)[name]; c.Status != checkSkip || c.Message != "not a desktop image" {
			t.Fatalf("%s = %+v, want skip", name, c)
		}
	}
}

// a desktop image missing one piece reports that piece, not just the first
// failure, and keeps going.
func TestDoctorReportsEachDisplayPiece(t *testing.T) {
	d := testDoctor(t)
	d.comp.pieces = func() []displayPiece {
		return []displayPiece{
			{name: "xdotool"},
			{name: "scrot", err: errors.New("scrot not installed")},
			{name: "display", err: errors.New("display :1 is not up")},
		}
	}
	got := checksByName(d.report(context.Background()))
	if got["xdotool"].Status != checkPass || got["scrot"].Status != checkFail || got["display"].Status != checkFail {
		t.Fatalf("pieces = %+v %+v %+v", got["xdotool"], got["scrot"], got["display"])
	}
	if got["wm"].Status != checkSkip || got["vnc"].Status != checkSkip {
		t.Fatalf("wm/vnc cannot be judged without a display: %+v %+v", got["wm"], got["vnc"])
	}
	if got["disk"].Status != checkPass || got["egress"].Status != checkPass {
		t.Fatal("a display failure must not stop the later checks")
	}
}

func TestCheckReadyNamesEveryMissingPiece(t *testing.T) {
	c := newComputer(":1")
	c.pieces = func() []displayPiece {
		return []displayPiece{
			{name: "xdotool", err: errors.New("xdotool not installed")},
			{name: "scrot"},
			{name: "display", err: errors.New("display :1 is not up")},
		}
	}
	err := c.ready()
	if err == nil || err.Error() != "xdotool not installed; display :1 is not up" {
		t.Fatalf("ready = %v", err)
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
		healthPassing:  checkPass,
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

func TestDoctorMachineID(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "short": "abc\n", "not hex": strings.Repeat("z", 32)} {
		d := testDoctor(t)
		if err := os.WriteFile(d.machineIDPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if s := checksByName(d.report(context.Background()))["machine-id"].Status; s != checkFail {
			t.Fatalf("%s machine-id = %s, want fail", name, s)
		}
	}
	d := testDoctor(t)
	d.machineIDPath = filepath.Join(t.TempDir(), "missing")
	if s := checksByName(d.report(context.Background()))["machine-id"].Status; s != checkFail {
		t.Fatalf("missing machine-id = %s, want fail", s)
	}
}

func TestDoctorNoDBusIsWarn(t *testing.T) {
	d := testDoctor(t)
	d.dbusPath = filepath.Join(t.TempDir(), "missing")
	if s := checksByName(d.report(context.Background()))["dbus"].Status; s != checkWarn {
		t.Fatalf("dbus = %s, want warn", s)
	}
}

func TestDoctorNetworkDown(t *testing.T) {
	d := testDoctor(t)
	d.lookup = func(context.Context, string) error { return errors.New("no such host") }
	d.fetch = func(context.Context, string, *url.URL) (time.Time, error) {
		return time.Time{}, errors.New("connection refused")
	}
	got := checksByName(d.report(context.Background()))
	if got["dns"].Status != checkFail || got["egress"].Status != checkFail {
		t.Fatalf("dns/egress = %+v %+v", got["dns"], got["egress"])
	}
	if got["clock"].Status != checkSkip {
		t.Fatalf("clock without egress = %+v, want skip", got["clock"])
	}
}

func TestDoctorClockSkew(t *testing.T) {
	for skew, want := range map[time.Duration]string{
		2 * time.Second:   checkPass,
		-30 * time.Second: checkWarn,
		10 * time.Minute:  checkFail,
	} {
		d := testDoctor(t)
		d.fetch = func(context.Context, string, *url.URL) (time.Time, error) { return testNow.Add(-skew), nil }
		if c := checksByName(d.report(context.Background()))["clock"]; c.Status != want {
			t.Fatalf("skew %s = %+v, want %s", skew, c, want)
		}
	}
}

// under proxied egress the guest resolver is off the path, and the egress
// request must go through the proxy the workload uses.
func TestDoctorProxiedEgress(t *testing.T) {
	d := testDoctor(t)
	if err := os.WriteFile(d.egressPath, []byte(`{"mode":"proxy","protocol":"socks5","endpoint":"socks5h://10.0.0.1:1080"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotProxy *url.URL
	d.lookup = func(context.Context, string) error {
		t.Error("dns must not be checked under proxied egress")
		return nil
	}
	d.fetch = func(_ context.Context, _ string, proxy *url.URL) (time.Time, error) {
		gotProxy = proxy
		return testNow, nil
	}
	got := checksByName(d.report(context.Background()))
	if got["dns"].Status != checkSkip {
		t.Fatalf("dns = %+v, want skip", got["dns"])
	}
	if gotProxy == nil || gotProxy.String() != "socks5h://10.0.0.1:1080" {
		t.Fatalf("egress went via %v, want the socks5 proxy", gotProxy)
	}
	if got["egress"].Message != "reached example.com via socks5h://10.0.0.1:1080" {
		t.Fatalf("egress message = %q", got["egress"].Message)
	}
}

func TestEgressProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "egress.json")
	if u, err := egressProxy(path); u != nil || err != nil {
		t.Fatalf("missing file = %v %v, want direct", u, err)
	}
	for body, want := range map[string]string{
		`{"mode":"direct"}`: "",
		`{"mode":"proxy","protocol":"http","endpoint":"10.0.0.1:3128"}`: "http://10.0.0.1:3128",
		`{"mode":"proxy","endpoint":"10.0.0.1:1080"}`:                   "socks5://10.0.0.1:1080",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		u, err := egressProxy(path)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := ""; u != nil {
			got = u.String()
			if got != want {
				t.Fatalf("%s -> %s, want %s", body, got, want)
			}
		} else if want != "" {
			t.Fatalf("%s -> direct, want %s", body, want)
		}
	}
	if err := os.WriteFile(path, []byte(`{"mode":"proxy"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := egressProxy(path); err == nil {
		t.Fatal("a proxied policy with no endpoint must be an error")
	}
}

func TestFetchDateReadsHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", testNow.Format(http.TimeFormat))
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	got, err := fetchDate(context.Background(), srv.URL, nil)
	if err != nil || !got.Equal(testNow) {
		t.Fatalf("fetchDate = %v %v, want %v (any status counts as reached)", got, err, testNow)
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
		{50, 100, checkPass},
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

func TestDoctorLogReportWritesOneLinePerCheck(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	d := testDoctor(t)
	d.dial = func(string) error { return errors.New("refused") }
	d.lookup = func(context.Context, string) error { t.Error("the boot run must not touch the network"); return nil }
	d.fetch = func(context.Context, string, *url.URL) (time.Time, error) {
		t.Error("the boot run must not touch the network")
		return testNow, nil
	}
	d.logReport()
	out := buf.String()
	for _, want := range []string{"doctor: PASS display: :1 1024x768", "doctor: FAIL vnc: vnc server not reachable", "doctor: SKIP healthcheck: no healthcheck declared", "doctor: SKIP egress: not run at boot", "doctor: FAIL\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorRouteIsProtected(t *testing.T) {
	// an unparseable egress file keeps the real doctor off the network: the
	// egress check fails on the file instead of dialing out from ci.
	egress := filepath.Join(t.TempDir(), "egress.json")
	if err := os.WriteFile(egress, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1", egress: egress}, newCredentials("secret-token"), nil, 0, false, nil))
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
