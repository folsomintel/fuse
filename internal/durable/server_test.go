package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// fakeFleet knows one vm and one host.
type fakeFleet struct{}

func (fakeFleet) GetVM(id string) (orchestrator.VMInfo, bool) {
	return orchestrator.VMInfo{ID: id}, id == "fuse-known"
}

func (fakeFleet) GetHost(id string) (orchestrator.Host, bool) {
	return orchestrator.Host{ID: id, Token: "secret"}, id == "host-known"
}

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// discovery is the manifest restate-server reads on registration. checking it
// needs no restate-server, so it runs in every go test.
func TestDiscoveryManifest(t *testing.T) {
	cfg := Config{Mode: ModeSelf, Retry: map[string]RetryPolicy{
		EnvironmentObject: DefaultRetryPolicy,
		HostObject:        {InitialInterval: 2 * time.Second, MaxInterval: time.Minute, MaxAttempts: 3},
	}}
	h, err := Handler(cfg, fakeFleet{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/discover", nil)
	req.Header.Set("Accept", "application/vnd.restate.endpointmanifest.v4+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("discover: http %d: %s", res.StatusCode, body)
	}

	var manifest struct {
		Services []struct {
			Name     string `json:"name"`
			Ty       string `json:"ty"`
			Handlers []struct {
				Name string `json:"name"`
				Ty   string `json:"ty"`
			} `json:"handlers"`
			RetryPolicyMaxAttempts     *int   `json:"retryPolicyMaxAttempts"`
			RetryPolicyInitialInterval *int64 `json:"retryPolicyInitialInterval"`
		} `json:"services"`
	}
	if err := json.NewDecoder(res.Body).Decode(&manifest); err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	for _, s := range manifest.Services {
		if s.Ty != "VIRTUAL_OBJECT" {
			t.Errorf("%s: type %q, want VIRTUAL_OBJECT", s.Name, s.Ty)
		}
		if len(s.Handlers) != 1 || s.Handlers[0].Name != "Ping" || s.Handlers[0].Ty != "SHARED" {
			t.Errorf("%s: handlers %+v, want one shared Ping", s.Name, s.Handlers)
		}
		if s.RetryPolicyMaxAttempts != nil {
			got[s.Name] = *s.RetryPolicyMaxAttempts
		}
	}
	want := map[string]int{EnvironmentObject: DefaultRetryPolicy.MaxAttempts, HostObject: 3}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s: retry max attempts %d, want %d (manifest services: %+v)", name, got[name], n, manifest.Services)
		}
	}
}

// TestPingThroughRestate runs the endpoint against a real restate-server,
// registers it and pings both objects through ingress. it needs a running
// restate-server, so it is skipped unless both urls are set:
//
//	FUSE_RESTATE_TEST_ADMIN_URL=http://127.0.0.1:9070 \
//	FUSE_RESTATE_TEST_INGRESS_URL=http://127.0.0.1:18080 \
//	  go test ./internal/durable -run Through
func TestPingThroughRestate(t *testing.T) {
	admin := os.Getenv("FUSE_RESTATE_TEST_ADMIN_URL")
	ingressURL := os.Getenv("FUSE_RESTATE_TEST_INGRESS_URL")
	if admin == "" || ingressURL == "" {
		t.Skip("set FUSE_RESTATE_TEST_ADMIN_URL and FUSE_RESTATE_TEST_INGRESS_URL to run against restate-server")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := Config{Mode: ModeSelf, Listen: addr, IngressURL: ingressURL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, cfg, fakeFleet{}, testLogger) }()

	// restate-server dials the endpoint during registration, so wait for it
	// to accept connections first.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoint never listened on %s", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}

	body := fmt.Sprintf(`{"uri":"http://%s","force":true}`, addr)
	res, err := http.Post(admin+"/deployments", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode/100 != 2 {
		t.Fatalf("register: http %d: %s", res.StatusCode, reg)
	}
	if !strings.Contains(string(reg), EnvironmentObject) || !strings.Contains(string(reg), HostObject) {
		t.Fatalf("register response lacks the services: %s", reg)
	}

	client := NewClient(ingressURL)
	if err := client.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}

	cases := []struct {
		object, key string
		known       bool
	}{
		{EnvironmentObject, "fuse-known", true},
		{EnvironmentObject, "fuse-missing", false},
		{HostObject, "host-known", true},
		{HostObject, "host-missing", false},
	}
	for _, c := range cases {
		got, err := client.Ping(ctx, c.object, c.key)
		if err != nil {
			t.Fatalf("ping %s/%s: %v", c.object, c.key, err)
		}
		want := PingResult{Object: c.object, Key: c.key, Known: c.known}
		if got != want {
			t.Errorf("ping %s/%s = %+v, want %+v", c.object, c.key, got, want)
		}
	}
}

func TestCheckUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	_ = ln.Close()
	if err := NewClient(url).Check(context.Background()); err == nil {
		t.Fatal("check against a closed port returned nil")
	}
}
