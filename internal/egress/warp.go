package egress

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// WarpName is the provider name a caller uses to ask for cloudflare warp.
const WarpName = "cloudflare-warp"

// WarpConfig is what the orchestrator holds for the cloudflare warp backend:
// the credential the host needs to enrol its warp-svc, and nothing else.
// either a zero trust service token (organization, client id, client secret,
// enrolled headlessly through mdm.xml) or a consumer warp+ license. an
// orchestrator with neither does not register the provider at all, so a
// caller asking for it gets "unknown provider" rather than a boot failure.
type WarpConfig struct {
	Organization     string
	AuthClientID     string
	AuthClientSecret string
	License          string
	// ProxyPort is the port warp-svc listens on inside its namespace on the
	// host. zero means the host's default (40000).
	ProxyPort int
}

// Enabled reports whether the config carries a usable credential.
func (c WarpConfig) Enabled() bool {
	return c.License != "" || (c.Organization != "" && c.AuthClientID != "" && c.AuthClientSecret != "")
}

// Warp is the control-plane half of cloudflare warp: it owns the contract
// and the credential, and drives the host agent through the generic egress
// wire. the host owns the process. nothing in the boot path names warp.
type Warp struct {
	cfg WarpConfig
}

// NewWarp returns a provider holding cfg.
func NewWarp(cfg WarpConfig) *Warp {
	return &Warp{cfg: cfg}
}

// Name implements Provider.
func (w *Warp) Name() string { return WarpName }

// config is the map the host agent's warp backend reads. the keys are the
// backend's contract (fc-warp.sh reads them as KEY=value lines on stdin).
func (w *Warp) config() map[string]string {
	cfg := map[string]string{}
	if w.cfg.License != "" {
		cfg["WARP_LICENSE"] = w.cfg.License
	}
	if w.cfg.Organization != "" {
		cfg["WARP_ORG"] = w.cfg.Organization
		cfg["WARP_CLIENT_ID"] = w.cfg.AuthClientID
		cfg["WARP_CLIENT_SECRET"] = w.cfg.AuthClientSecret
	}
	if w.cfg.ProxyPort > 0 {
		cfg["WARP_PROXY_PORT"] = strconv.Itoa(w.cfg.ProxyPort)
	}
	return cfg
}

// Provision implements Provider: it asks the vm's host to bring warp up
// for the vm and returns the endpoint the host published on the tap.
func (w *Warp) Provision(ctx context.Context, ec Context) (Endpoint, error) {
	if ec.Host == nil {
		return Endpoint{}, errors.New("cloudflare-warp: the vm's provider exposes no egress wire; the backend runs on a firecracker host")
	}
	if ec.ListenIP == "" {
		return Endpoint{}, errors.New("cloudflare-warp: the vm reported no tap address to publish the proxy on")
	}
	req := HostRequest{
		Provider: WarpName,
		Protocol: ec.Protocol,
		ListenIP: ec.ListenIP,
		GuestIP:  ec.GuestIP,
		Config:   w.config(),
	}
	ep, err := ec.Host.ProvisionEgress(ctx, ec.VMID, req)
	if err != nil {
		return Endpoint{}, w.redact(err)
	}
	ep.Provider = WarpName
	if ep.Protocol == "" {
		ep.Protocol = ec.Protocol
	}
	return ep, nil
}

// Healthcheck implements Provider.
func (w *Warp) Healthcheck(ctx context.Context, ep Endpoint) error {
	return ProbeEndpoint(ctx, ep)
}

// Destroy implements Provider. a vm with no host has nothing to release.
func (w *Warp) Destroy(ctx context.Context, ec Context) error {
	if ec.Host == nil {
		return nil
	}
	if err := ec.Host.DestroyEgress(ctx, ec.VMID); err != nil {
		return w.redact(err)
	}
	return nil
}

// redact strips every credential value out of an error before it leaves
// this provider. the fleet redacts task secrets, not orchestrator config,
// so nothing upstream would catch a host agent echoing the request back.
func (w *Warp) redact(err error) error {
	msg := err.Error()
	for _, v := range []string{w.cfg.License, w.cfg.AuthClientSecret, w.cfg.AuthClientID} {
		if len(v) >= 8 {
			msg = strings.ReplaceAll(msg, v, "[REDACTED]")
		}
	}
	return fmt.Errorf("cloudflare-warp: %s", msg)
}
