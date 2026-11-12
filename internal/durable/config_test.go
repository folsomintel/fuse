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
		{"cloud refused", map[string]string{"FUSE_RESTATE_MODE": "cloud"}, "not supported yet"},
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
