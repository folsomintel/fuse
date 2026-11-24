package fuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoctor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments/vm-1/doctor" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"fail","vm_id":"vm-1","version":"v1.4.0","checks":[` +
			`{"name":"display","status":"pass","message":":1 1280x800"},` +
			`{"name":"vnc","status":"fail","message":"vnc server not reachable"},` +
			`{"name":"healthcheck","status":"skip"}]}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Environments.Doctor(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if res.Status != DoctorFail || res.Version != "v1.4.0" || len(res.Checks) != 3 {
		t.Fatalf("report = %+v", res)
	}
	if res.Checks[0].Name != "display" || res.Checks[0].Message != ":1 1280x800" || res.Checks[2].Status != DoctorSkip {
		t.Fatalf("checks = %+v", res.Checks)
	}

	if _, err := c.Environments.Doctor(context.Background(), ""); err == nil {
		t.Fatal("empty vm id must be rejected before any request is made")
	}
}
