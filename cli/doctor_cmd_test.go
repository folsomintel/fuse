package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doctorServer(t *testing.T, report string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments/vm1/doctor" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, report)
	}))
}

func runDoctor(t *testing.T, srvURL string, extra ...string) (string, int) {
	t.Helper()
	cfg := writeConfig(t, srvURL)
	app.exitCode = 0
	t.Cleanup(func() { app.exitCode = 0 })
	stdout, _, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs(append([]string{"--config", cfg, "doctor", "vm1"}, extra...))
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return stdout, app.exitCode
}

func TestDoctorPrintsChecks(t *testing.T) {
	srv := doctorServer(t, `{"status":"warn","version":"v1.4.0","checks":[`+
		`{"name":"display","status":"warn","message":":1 1024x768, desktop declares 1280x800"},`+
		`{"name":"healthcheck","status":"skip","message":"no healthcheck declared"}]}`)
	defer srv.Close()

	out, code := runDoctor(t, srv.URL)
	if code != 0 {
		t.Fatalf("warn must exit 0, got %d", code)
	}
	for _, want := range []string{"WARN display: :1 1024x768, desktop declares 1280x800", "SKIP healthcheck: no healthcheck declared"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorFailExitsNonZero(t *testing.T) {
	srv := doctorServer(t, `{"status":"fail","version":"v1.4.0","checks":[`+
		`{"name":"vnc","status":"fail","message":"vnc server not reachable"}]}`)
	defer srv.Close()

	out, code := runDoctor(t, srv.URL, "-o", "json")
	if code != 1 {
		t.Fatalf("fail must exit 1, got %d", code)
	}
	if !strings.Contains(out, `"status": "fail"`) {
		t.Fatalf("json output = %s", out)
	}
}
