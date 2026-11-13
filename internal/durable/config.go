// Package durable runs the orchestrator's restate handlers and talks to
// restate ingress.
//
// restate-server sits in front of the handlers here and journals every step,
// so an operation that crashes halfway is replayed from its last finished step
// instead of being lost. nothing is ported onto it yet: this package only
// stands up the handler endpoint, the ingress client and the two virtual
// objects (Environment, Host) later work fills in.
package durable

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mode is where restate-server runs, from FUSE_RESTATE_MODE.
type Mode string

const (
	// ModeOff runs no handlers and calls no restate. the default, and
	// exactly the orchestrator as it was before restate.
	ModeOff Mode = "off"
	// ModeSelf talks to a restate-server the operator runs next to the
	// orchestrator (docker, or the binary on the local appliance).
	ModeSelf Mode = "self"
	// ModeCloud talks to a restate cloud environment. the handler endpoint
	// rejects any request the environment did not sign.
	ModeCloud Mode = "cloud"
)

// ParseMode reads a FUSE_RESTATE_MODE value. empty means off.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeOff, nil
	case ModeOff, ModeSelf, ModeCloud:
		return m, nil
	default:
		return "", fmt.Errorf("FUSE_RESTATE_MODE: want off, self or cloud; got %q", s)
	}
}

// Connect is how restate cloud reaches the handler endpoint, from
// FUSE_RESTATE_CONNECT. only read in mode cloud.
type Connect string

const (
	// ConnectTunnel dials out to restate cloud's tunnel servers
	// (sdk-go/x/tunnel), so the orchestrator needs no inbound port.
	ConnectTunnel Connect = "tunnel"
	// ConnectPublic serves the handler endpoint on ORCH_RESTATE_LISTEN behind
	// a tls proxy, and registers FUSE_RESTATE_DEPLOYMENT_URL with cloud. the
	// fallback while the tunnel is pre-1.0.
	ConnectPublic Connect = "public"
)

// DefaultTunnelName is the tunnel name when FUSE_RESTATE_TUNNEL_NAME is unset.
const DefaultTunnelName = "fuse-orchestrator"

// cloudHost matches a restate cloud ingress host, <id>.env.<region>.restate.cloud.
var cloudHost = regexp.MustCompile(`^([a-z0-9]+)\.env\.([a-z0-9-]+)\.restate\.cloud$`)

// DefaultListen is where the handler endpoint listens. loopback, because in
// self mode restate-server runs on the same machine and nothing else should
// be able to invoke handlers.
const DefaultListen = "127.0.0.1:9080"

// RetryPolicy is the invocation retry policy restate applies to one service:
// exponential backoff from InitialInterval, capped at MaxInterval, giving up
// after MaxAttempts (the first call counts as an attempt).
type RetryPolicy struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxAttempts     int
}

// DefaultRetryPolicy is what every service gets unless the env overrides it.
var DefaultRetryPolicy = RetryPolicy{
	InitialInterval: 500 * time.Millisecond,
	MaxInterval:     30 * time.Second,
	MaxAttempts:     10,
}

// Config is the durable layer's configuration.
type Config struct {
	Mode Mode
	// Listen is the handler endpoint address restate-server invokes.
	Listen string
	// IngressURL is restate's ingress, where the orchestrator starts and
	// attaches to invocations, e.g. http://127.0.0.1:18080.
	IngressURL string
	// Retry holds one policy per service name (EnvironmentObject, HostObject).
	Retry map[string]RetryPolicy

	// the rest is mode cloud only.

	Connect Connect
	// APIKey is the restate cloud api key, sent as a bearer token to ingress
	// and to the admin api for registration.
	APIKey string
	// IdentityKey is the environment's request signing public key
	// (publickeyv1_...). requests without a valid signature get a 401.
	IdentityKey string
	// AdminURL is the cloud admin api the deployment registers with.
	AdminURL string
	// DeploymentURL is the https url cloud reaches the endpoint at (public).
	DeploymentURL string
	// EnvironmentID, Region, TunnelName and TokenFile configure the tunnel.
	EnvironmentID string
	Region        string
	TunnelName    string
	// TokenFile, when set, is where the tunnel reads its api key on every
	// reconnect, so a rotated key needs no restart. APIKey still serves
	// ingress and registration.
	TokenFile string
}

// LoadConfig reads the config from the environment through getenv
// (os.Getenv in main, a map in tests). mode off ignores every other variable.
//
//	FUSE_RESTATE_MODE          off | self | cloud (default off)
//	FUSE_RESTATE_INGRESS_URL   restate ingress url (required unless off)
//	ORCH_RESTATE_LISTEN        handler endpoint (default 127.0.0.1:9080)
//	FUSE_RESTATE_RETRY_<SERVICE>_INITIAL_INTERVAL   e.g. 500ms
//	FUSE_RESTATE_RETRY_<SERVICE>_MAX_INTERVAL       e.g. 30s
//	FUSE_RESTATE_RETRY_<SERVICE>_MAX_ATTEMPTS       e.g. 10
//
// <SERVICE> is the upper-cased service name: ENVIRONMENT or HOST.
//
// mode cloud also reads:
//
//	FUSE_RESTATE_API_KEY            cloud api key (required)
//	FUSE_RESTATE_IDENTITY_KEY       publickeyv1_... signing key (required)
//	FUSE_RESTATE_CONNECT            tunnel | public (default tunnel)
//	FUSE_RESTATE_ADMIN_URL          default: the ingress host on port 9070
//	FUSE_RESTATE_DEPLOYMENT_URL     https url of the endpoint (public only)
//	FUSE_RESTATE_ENVIRONMENT_ID     env_... (tunnel; default from the ingress host)
//	FUSE_RESTATE_REGION             us, eu, ... (tunnel; default from the ingress host)
//	FUSE_RESTATE_TUNNEL_NAME        default fuse-orchestrator
//	RESTATE_INPROC_AUTH_TOKEN_FILE  api key file the tunnel re-reads on reconnect
func LoadConfig(getenv func(string) string) (Config, error) {
	mode, err := ParseMode(getenv("FUSE_RESTATE_MODE"))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Mode: mode}
	if mode == ModeOff {
		return cfg, nil
	}
	cfg.Listen = getenv("ORCH_RESTATE_LISTEN")
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}

	cfg.IngressURL = strings.TrimRight(getenv("FUSE_RESTATE_INGRESS_URL"), "/")
	if cfg.IngressURL == "" {
		return Config{}, fmt.Errorf("FUSE_RESTATE_MODE=%s needs FUSE_RESTATE_INGRESS_URL", mode)
	}
	u, err := url.Parse(cfg.IngressURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Config{}, fmt.Errorf("FUSE_RESTATE_INGRESS_URL: want an http(s) url, got %q", cfg.IngressURL)
	}

	cfg.Retry = make(map[string]RetryPolicy, len(services))
	for _, name := range services {
		p, err := loadRetryPolicy(getenv, name)
		if err != nil {
			return Config{}, err
		}
		cfg.Retry[name] = p
	}

	if mode == ModeCloud {
		if err := loadCloud(getenv, &cfg, u); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// loadCloud fills and checks the mode cloud fields. ingress is the parsed
// FUSE_RESTATE_INGRESS_URL.
func loadCloud(getenv func(string) string, cfg *Config, ingress *url.URL) error {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }

	if ingress.Scheme != "https" {
		return fmt.Errorf("FUSE_RESTATE_MODE=cloud needs an https FUSE_RESTATE_INGRESS_URL, got %q", cfg.IngressURL)
	}
	cfg.APIKey = get("FUSE_RESTATE_API_KEY")
	if cfg.APIKey == "" {
		return errors.New("FUSE_RESTATE_MODE=cloud needs FUSE_RESTATE_API_KEY")
	}
	cfg.IdentityKey = get("FUSE_RESTATE_IDENTITY_KEY")
	if !strings.HasPrefix(cfg.IdentityKey, "publickeyv1_") {
		return fmt.Errorf("FUSE_RESTATE_MODE=cloud needs FUSE_RESTATE_IDENTITY_KEY=publickeyv1_..., got %q", cfg.IdentityKey)
	}

	cfg.AdminURL = strings.TrimRight(get("FUSE_RESTATE_ADMIN_URL"), "/")
	if cfg.AdminURL == "" {
		cfg.AdminURL = "https://" + ingress.Hostname() + ":9070"
	}
	if a, err := url.Parse(cfg.AdminURL); err != nil || a.Scheme != "https" || a.Host == "" {
		return fmt.Errorf("FUSE_RESTATE_ADMIN_URL: want an https url, got %q", cfg.AdminURL)
	}

	switch c := Connect(strings.ToLower(get("FUSE_RESTATE_CONNECT"))); c {
	case "", ConnectTunnel:
		cfg.Connect = ConnectTunnel
	case ConnectPublic:
		cfg.Connect = ConnectPublic
	default:
		return fmt.Errorf("FUSE_RESTATE_CONNECT: want tunnel or public, got %q", c)
	}

	if cfg.Connect == ConnectPublic {
		cfg.DeploymentURL = strings.TrimRight(get("FUSE_RESTATE_DEPLOYMENT_URL"), "/")
		d, err := url.Parse(cfg.DeploymentURL)
		if err != nil || d.Scheme != "https" || d.Host == "" {
			return fmt.Errorf("FUSE_RESTATE_CONNECT=public needs an https FUSE_RESTATE_DEPLOYMENT_URL (restate cloud only calls tls endpoints), got %q", cfg.DeploymentURL)
		}
		return nil
	}

	// tunnel: environment id and region default to what the ingress host
	// says, which is every restate-managed region; byoc sets them.
	var hostID, hostRegion string
	if m := cloudHost.FindStringSubmatch(ingress.Hostname()); m != nil {
		hostID, hostRegion = "env_"+m[1], m[2]
	}
	cfg.EnvironmentID = get("FUSE_RESTATE_ENVIRONMENT_ID")
	if cfg.EnvironmentID == "" {
		cfg.EnvironmentID = hostID
	}
	cfg.Region = get("FUSE_RESTATE_REGION")
	if cfg.Region == "" {
		cfg.Region = hostRegion
	}
	if !strings.HasPrefix(cfg.EnvironmentID, "env_") || cfg.Region == "" {
		return fmt.Errorf("FUSE_RESTATE_CONNECT=tunnel needs FUSE_RESTATE_ENVIRONMENT_ID (env_...) and FUSE_RESTATE_REGION; %s is not a <id>.env.<region>.restate.cloud host to read them from", ingress.Hostname())
	}
	cfg.TunnelName = get("FUSE_RESTATE_TUNNEL_NAME")
	if cfg.TunnelName == "" {
		cfg.TunnelName = DefaultTunnelName
	}
	cfg.TokenFile = get("RESTATE_INPROC_AUTH_TOKEN_FILE")
	return nil
}

// loadRetryPolicy starts from DefaultRetryPolicy and applies any
// FUSE_RESTATE_RETRY_<SERVICE>_* overrides.
func loadRetryPolicy(getenv func(string) string, service string) (RetryPolicy, error) {
	p := DefaultRetryPolicy
	prefix := "FUSE_RESTATE_RETRY_" + strings.ToUpper(service) + "_"

	for _, f := range []struct {
		name string
		dst  *time.Duration
	}{
		{prefix + "INITIAL_INTERVAL", &p.InitialInterval},
		{prefix + "MAX_INTERVAL", &p.MaxInterval},
	} {
		v := getenv(f.name)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return RetryPolicy{}, fmt.Errorf("%s: want a positive duration like 500ms or 30s, got %q", f.name, v)
		}
		*f.dst = d
	}

	if v := getenv(prefix + "MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return RetryPolicy{}, fmt.Errorf("%sMAX_ATTEMPTS: want a whole number >= 1, got %q", prefix, v)
		}
		p.MaxAttempts = n
	}

	if p.MaxInterval < p.InitialInterval {
		return RetryPolicy{}, fmt.Errorf("%sMAX_INTERVAL (%s) is below %sINITIAL_INTERVAL (%s)",
			prefix, p.MaxInterval, prefix, p.InitialInterval)
	}
	return p, nil
}
