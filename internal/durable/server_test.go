package durable

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/mr-tron/base58"

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
			// intervals are milliseconds on the wire.
			RetryPolicyMaxAttempts     *int   `json:"retryPolicyMaxAttempts"`
			RetryPolicyInitialInterval *int64 `json:"retryPolicyInitialInterval"`
			RetryPolicyMaxInterval     *int64 `json:"retryPolicyMaxInterval"`
		} `json:"services"`
	}
	if err := json.NewDecoder(res.Body).Decode(&manifest); err != nil {
		t.Fatal(err)
	}

	got := map[string]RetryPolicy{}
	for _, s := range manifest.Services {
		if s.Ty != "VIRTUAL_OBJECT" {
			t.Errorf("%s: type %q, want VIRTUAL_OBJECT", s.Name, s.Ty)
		}
		if len(s.Handlers) != 1 || s.Handlers[0].Name != "Ping" || s.Handlers[0].Ty != "SHARED" {
			t.Errorf("%s: handlers %+v, want one shared Ping", s.Name, s.Handlers)
		}
		var p RetryPolicy
		if s.RetryPolicyMaxAttempts != nil {
			p.MaxAttempts = *s.RetryPolicyMaxAttempts
		}
		if s.RetryPolicyInitialInterval != nil {
			p.InitialInterval = time.Duration(*s.RetryPolicyInitialInterval) * time.Millisecond
		}
		if s.RetryPolicyMaxInterval != nil {
			p.MaxInterval = time.Duration(*s.RetryPolicyMaxInterval) * time.Millisecond
		}
		got[s.Name] = p
	}
	for name, want := range cfg.Retry {
		if got[name] != want {
			t.Errorf("%s: manifest retry policy %+v, want %+v", name, got[name], want)
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
//
// FUSE_RESTATE_TEST_IDENTITY_KEY=publickeyv1_... also turns on request
// identity, for a restate-server started with
// RESTATE_REQUEST_IDENTITY_PRIVATE_KEY_PEM_FILE: every call then has to carry
// its signature.
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

	cfg := Config{Mode: ModeSelf, Listen: addr, IngressURL: ingressURL, AdminURL: admin,
		IdentityKey: os.Getenv("FUSE_RESTATE_TEST_IDENTITY_KEY")}
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

	client := NewClient(cfg)
	if err := client.Register(ctx, "http://"+addr); err != nil {
		t.Fatalf("register: %v", err)
	}
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
	if err := NewClient(Config{IngressURL: url}).Check(context.Background()); err == nil {
		t.Fatal("check against a closed port returned nil")
	}
}

// signRequest returns the X-Restate-Jwt-V1 value restate cloud would send for
// path, signed with priv.
func signRequest(t *testing.T, priv ed25519.PrivateKey, path string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{path},
		NotBefore: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	})
	tok.Header["kid"] = identityKey(priv.Public().(ed25519.PublicKey))
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// identityKey is the publickeyv1_ form restate prints for a public key.
func identityKey(pub ed25519.PublicKey) string {
	return "publickeyv1_" + base58.Encode(pub)
}

// with an identity key set, the endpoint only answers requests the matching
// private key signed: what keeps a public handler url from being invoked by
// anyone but the restate cloud environment.
func TestIdentityKeyRejectsUnsignedRequests(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)

	cfg := Config{Mode: ModeCloud, IdentityKey: identityKey(pub)}
	h, err := Handler(cfg, fakeFleet{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	cases := []struct {
		name string
		jwt  string
		want int
	}{
		{"unsigned", "", http.StatusUnauthorized},
		{"signed by another key", signRequest(t, other, "/discover"), http.StatusUnauthorized},
		{"signed for another path", signRequest(t, priv, "/invoke/Host/Ping"), http.StatusUnauthorized},
		{"signed by the environment", signRequest(t, priv, "/discover"), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/discover", nil)
			req.Header.Set("Accept", "application/vnd.restate.endpointmanifest.v4+json")
			if c.jwt != "" {
				req.Header.Set("X-Restate-Signature-Scheme", "v1")
				req.Header.Set("X-Restate-Jwt-V1", c.jwt)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != c.want {
				t.Fatalf("discover: http %d, want %d", res.StatusCode, c.want)
			}
		})
	}
}

func TestIdentityKeyInvalidFailsAtStart(t *testing.T) {
	cfg := Config{Mode: ModeCloud, IdentityKey: "publickeyv1_notbase58!"}
	if _, err := Handler(cfg, fakeFleet{}, testLogger); err == nil {
		t.Fatal("a malformed identity key built a handler")
	}
}

// the api key goes to ingress health and to registration as a bearer token,
// and registration forces the uri in place.
func TestClientSendsAPIKey(t *testing.T) {
	var gotAuth []string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.URL.Path+" "+r.Header.Get("Authorization"))
		if r.URL.Path == "/deployments" {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{IngressURL: srv.URL, AdminURL: srv.URL, APIKey: "key_abc"})
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Register(context.Background(), "https://tunnel.example/env/fuse"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/restate/health Bearer key_abc", "/deployments Bearer key_abc"}
	if strings.Join(gotAuth, "|") != strings.Join(want, "|") {
		t.Errorf("auth = %v, want %v", gotAuth, want)
	}
	if gotBody["uri"] != "https://tunnel.example/env/fuse" || gotBody["force"] != true {
		t.Errorf("register body = %v", gotBody)
	}
}

func TestRegisterReportsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	err := NewClient(Config{AdminURL: srv.URL}).Register(context.Background(), "https://x")
	if err == nil || !strings.Contains(err.Error(), "http 401") {
		t.Fatalf("err = %v, want the 401", err)
	}
}
