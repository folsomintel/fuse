package durable

import (
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeOff, false},
		{"off", ModeOff, false},
		{" Self ", ModeSelf, false},
		{"cloud", ModeCloud, false},
		{"on", "", true},
		{"selfhosted", "", true},
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestLoadConfigOffIgnoresEverythingElse(t *testing.T) {
	cfg, err := LoadConfig(envMap(map[string]string{
		"FUSE_RESTATE_INGRESS_URL":                    "not a url",
		"FUSE_RESTATE_RETRY_ENVIRONMENT_MAX_ATTEMPTS": "zero",
	}))
	if err != nil {
		t.Fatalf("mode off must not validate the other vars: %v", err)
	}
	if cfg.Mode != ModeOff {
		t.Fatalf("mode = %q, want off", cfg.Mode)
	}
}

func TestLoadConfigSelf(t *testing.T) {
	cfg, err := LoadConfig(envMap(map[string]string{
		"FUSE_RESTATE_MODE":        "self",
		"FUSE_RESTATE_INGRESS_URL": "http://127.0.0.1:18080/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", cfg.Listen, DefaultListen)
	}
	if cfg.IngressURL != "http://127.0.0.1:18080" {
		t.Errorf("ingress url = %q, want the trailing slash trimmed", cfg.IngressURL)
	}
	for _, s := range services {
		if cfg.Retry[s] != DefaultRetryPolicy {
			t.Errorf("retry[%s] = %+v, want the default", s, cfg.Retry[s])
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	base := map[string]string{
		"FUSE_RESTATE_MODE":        "self",
		"FUSE_RESTATE_INGRESS_URL": "http://127.0.0.1:18080",
	}
	cases := []struct {
		name    string
		set     map[string]string
		wantErr string
	}{
		{"bad mode", map[string]string{"FUSE_RESTATE_MODE": "yes"}, "want off, self or cloud"},
		{"no ingress", map[string]string{"FUSE_RESTATE_INGRESS_URL": ""}, "needs FUSE_RESTATE_INGRESS_URL"},
		{"ingress not http", map[string]string{"FUSE_RESTATE_INGRESS_URL": "127.0.0.1:18080"}, "want an http(s) url"},
		{"bad interval", map[string]string{"FUSE_RESTATE_RETRY_HOST_INITIAL_INTERVAL": "5"}, "HOST_INITIAL_INTERVAL"},
		{"negative interval", map[string]string{"FUSE_RESTATE_RETRY_HOST_MAX_INTERVAL": "-1s"}, "HOST_MAX_INTERVAL"},
		{"zero attempts", map[string]string{"FUSE_RESTATE_RETRY_ENVIRONMENT_MAX_ATTEMPTS": "0"}, "ENVIRONMENT_MAX_ATTEMPTS"},
		{"max below initial", map[string]string{
			"FUSE_RESTATE_RETRY_ENVIRONMENT_INITIAL_INTERVAL": "1m",
			"FUSE_RESTATE_RETRY_ENVIRONMENT_MAX_INTERVAL":     "10s",
		}, "is below"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range c.set {
				env[k] = v
			}
			_, err := LoadConfig(envMap(env))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestLoadConfigRetryOverrideIsPerService(t *testing.T) {
	cfg, err := LoadConfig(envMap(map[string]string{
		"FUSE_RESTATE_MODE":                        "self",
		"FUSE_RESTATE_INGRESS_URL":                 "http://127.0.0.1:18080",
		"ORCH_RESTATE_LISTEN":                      "127.0.0.1:9999",
		"FUSE_RESTATE_RETRY_HOST_INITIAL_INTERVAL": "2s",
		"FUSE_RESTATE_RETRY_HOST_MAX_INTERVAL":     "5m",
		"FUSE_RESTATE_RETRY_HOST_MAX_ATTEMPTS":     "3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	want := RetryPolicy{InitialInterval: 2 * time.Second, MaxInterval: 5 * time.Minute, MaxAttempts: 3}
	if cfg.Retry[HostObject] != want {
		t.Errorf("retry[Host] = %+v, want %+v", cfg.Retry[HostObject], want)
	}
	if cfg.Retry[EnvironmentObject] != DefaultRetryPolicy {
		t.Errorf("retry[Environment] = %+v, want the default untouched", cfg.Retry[EnvironmentObject])
	}
}

// cloudEnv is a complete mode cloud environment over the tunnel.
func cloudEnv(set map[string]string) map[string]string {
	env := map[string]string{
		"FUSE_RESTATE_MODE":         "cloud",
		"FUSE_RESTATE_INGRESS_URL":  "https://201hy10cd3h6426jy80tb32n6en.env.us.restate.cloud:8080",
		"FUSE_RESTATE_API_KEY":      "key_abc",
		"FUSE_RESTATE_IDENTITY_KEY": "publickeyv1_w7YHemBctH5Ck2nQRQ47iBBqhNHy4FV7t2Usbye2A6f",
	}
	for k, v := range set {
		env[k] = v
	}
	return env
}

func TestLoadConfigCloudTunnelDefaults(t *testing.T) {
	cfg, err := LoadConfig(envMap(cloudEnv(nil)))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"mode":           string(cfg.Mode) + " want cloud",
		"connect":        string(cfg.Connect) + " want tunnel",
		"admin url":      cfg.AdminURL + " want https://201hy10cd3h6426jy80tb32n6en.env.us.restate.cloud:9070",
		"environment id": cfg.EnvironmentID + " want env_201hy10cd3h6426jy80tb32n6en",
		"region":         cfg.Region + " want us",
		"tunnel name":    cfg.TunnelName + " want " + DefaultTunnelName,
	} {
		if v, w, _ := strings.Cut(got, " want "); v != w {
			t.Errorf("%s = %q, want %q", name, v, w)
		}
	}
}

func TestLoadConfigCloudOverrides(t *testing.T) {
	cfg, err := LoadConfig(envMap(cloudEnv(map[string]string{
		"FUSE_RESTATE_INGRESS_URL":       "https://ingress.byoc.example.com",
		"FUSE_RESTATE_ADMIN_URL":         "https://admin.byoc.example.com/",
		"FUSE_RESTATE_ENVIRONMENT_ID":    "env_byoc",
		"FUSE_RESTATE_REGION":            "byoc.example",
		"FUSE_RESTATE_TUNNEL_NAME":       "orch-blue",
		"RESTATE_INPROC_AUTH_TOKEN_FILE": "/run/fuse/restate-key",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminURL != "https://admin.byoc.example.com" || cfg.EnvironmentID != "env_byoc" ||
		cfg.Region != "byoc.example" || cfg.TunnelName != "orch-blue" || cfg.TokenFile != "/run/fuse/restate-key" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoadConfigCloudPublic(t *testing.T) {
	cfg, err := LoadConfig(envMap(cloudEnv(map[string]string{
		"FUSE_RESTATE_CONNECT":        "public",
		"FUSE_RESTATE_DEPLOYMENT_URL": "https://restate.orch.example.com/",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connect != ConnectPublic || cfg.DeploymentURL != "https://restate.orch.example.com" || cfg.Listen != DefaultListen {
		t.Fatalf("public config = %+v", cfg)
	}
}

func TestLoadConfigCloudErrors(t *testing.T) {
	cases := []struct {
		name    string
		set     map[string]string
		wantErr string
	}{
		{"plain http ingress", map[string]string{"FUSE_RESTATE_INGRESS_URL": "http://x.env.us.restate.cloud:8080"}, "https FUSE_RESTATE_INGRESS_URL"},
		{"no api key", map[string]string{"FUSE_RESTATE_API_KEY": ""}, "needs FUSE_RESTATE_API_KEY"},
		{"no identity key", map[string]string{"FUSE_RESTATE_IDENTITY_KEY": ""}, "FUSE_RESTATE_IDENTITY_KEY=publickeyv1_"},
		{"bad identity key", map[string]string{"FUSE_RESTATE_IDENTITY_KEY": "abc"}, "FUSE_RESTATE_IDENTITY_KEY=publickeyv1_"},
		{"bad connect", map[string]string{"FUSE_RESTATE_CONNECT": "vpn"}, "want tunnel or public"},
		{"http admin", map[string]string{"FUSE_RESTATE_ADMIN_URL": "http://admin:9070"}, "FUSE_RESTATE_ADMIN_URL"},
		{"public without url", map[string]string{"FUSE_RESTATE_CONNECT": "public"}, "https FUSE_RESTATE_DEPLOYMENT_URL"},
		{"public over http", map[string]string{
			"FUSE_RESTATE_CONNECT":        "public",
			"FUSE_RESTATE_DEPLOYMENT_URL": "http://orch:9080",
		}, "https FUSE_RESTATE_DEPLOYMENT_URL"},
		{"tunnel on a non-cloud host", map[string]string{"FUSE_RESTATE_INGRESS_URL": "https://ingress.example.com"}, "FUSE_RESTATE_ENVIRONMENT_ID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(envMap(cloudEnv(c.set)))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}
