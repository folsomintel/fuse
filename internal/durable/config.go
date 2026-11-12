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
	// ModeCloud talks to restate cloud. parsed so the value is reserved, but
	// refused until request signing is wired: a handler endpoint reachable
	// from the internet must reject requests cloud did not sign.
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
func LoadConfig(getenv func(string) string) (Config, error) {
	mode, err := ParseMode(getenv("FUSE_RESTATE_MODE"))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Mode: mode}
	if mode == ModeOff {
		return cfg, nil
	}
	if mode == ModeCloud {
		return Config{}, errors.New("FUSE_RESTATE_MODE=cloud is not supported yet; use self")
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
	return cfg, nil
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
