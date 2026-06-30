package fuse

import (
	"errors"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// sdkVersion reports the module version of this SDK as recorded in the consuming
// binary's build info. it is empty/"(devel)" in local builds and tests, where it
// falls back to "dev". stamped automatically by the go proxy for tagged installs.
func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	const modPath = "github.com/folsomintel/fuse/sdks/go"
	if info.Main.Path == modPath && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	for _, d := range info.Deps {
		if d.Path == modPath && d.Version != "" && d.Version != "(devel)" {
			return strings.TrimPrefix(d.Version, "v")
		}
	}
	return "dev"
}

const defaultTimeout = 60 * time.Second

// Client is the entry point for the Fuse API. It groups the resource
// services that share a single transport.
type Client struct {
	t            *transport
	Environments *EnvironmentsService
	Snapshots    *SnapshotsService
	Hosts        *HostsService
	APIKeys      *APIKeysService
}

// clientConfig holds the resolved options for New.
type clientConfig struct {
	httpClient *http.Client
	userAgent  string
	requestID  func() string
	baseURL    string
}

// Option configures a Client in New.
type Option func(*clientConfig)

// WithHTTPClient overrides the http.Client used for normal requests.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *clientConfig) {
		cfg.httpClient = c
	}
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(cfg *clientConfig) {
		cfg.userAgent = ua
	}
}

// WithRequestID sets a generator for the X-Request-ID header. It is
// called once per request; an empty return value is omitted.
func WithRequestID(fn func() string) Option {
	return func(cfg *clientConfig) {
		cfg.requestID = fn
	}
}

// WithBaseURL overrides the base URL passed to New.
func WithBaseURL(baseURL string) Option {
	return func(cfg *clientConfig) {
		cfg.baseURL = baseURL
	}
}

// New builds a Client. baseURL must be a non-empty, parseable URL.
// token is sent as a bearer token and may be empty for endpoints that
// do not require auth.
func New(baseURL, token string, opts ...Option) (*Client, error) {
	cfg := clientConfig{
		userAgent: "fuse-go/" + sdkVersion(),
		baseURL:   baseURL,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.baseURL == "" {
		return nil, errors.New("base url is required")
	}
	parsed, err := url.Parse(cfg.baseURL)
	if err != nil {
		return nil, errors.New("base url is invalid")
	}
	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	t := &transport{
		baseURL:    parsed,
		http:       httpClient,
		streamHTTP: &http.Client{Timeout: 0},
		bearer:     token,
		userAgent:  cfg.userAgent,
		requestID:  cfg.requestID,
	}
	c := &Client{
		t:            t,
		Environments: newEnvironmentsService(t),
		Snapshots:    newSnapshotsService(t),
		Hosts:        newHostsService(t),
		APIKeys:      newAPIKeysService(t),
	}
	return c, nil
}
