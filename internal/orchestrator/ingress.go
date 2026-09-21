package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/folsomintel/fuse/internal/tunnel"
)

// Stable ingress through fuse-proxy.
//
// without it an environment's url is a dnat port on whichever host it landed
// on, so moving the environment changes its url and every client has to find
// it again. with it the url is a port on the proxy, the guest dials out to the
// proxy (internal/tunnel), and nothing a client holds says where the guest is.
//
// the proxy is the source of truth for routes. the orchestrator asks for them,
// keeps the answer in memory, and asks again after a restart, so there is no
// second copy in the state store to drift from the first.
//
// a nil IngressProxy on FleetConfig means none of this happens and every url
// is the dnat one it always was.

// ErrIngressNotFound is returned by Lookup for an owner the proxy does not know.
var ErrIngressNotFound = errors.New("ingress owner not found")

// TunnelConfigGuestPath is where the sidecar's config lands in the guest. the
// host agent starts the sidecar when a start-agent request names it.
const TunnelConfigGuestPath = ReservedGuestDir + "/tunnel.json"

// ingressAgentRoute names the route to the guest agent's own port.
const ingressAgentRoute = "agent"

// fusedGuestPort is the port the guest agent listens on; see the agent profile.
const fusedGuestPort = 9550

// IngressPort is one guest tcp port to publish.
type IngressPort struct {
	Name      string
	GuestPort int
}

// IngressRequest publishes an owner's ports. the owner is the vm id.
type IngressRequest struct {
	Owner string
	// Token is the credential the guest will present. it is minted per vm, for
	// the reason fork mints everything else per vm: a disk copy must not be
	// able to speak as its source.
	Token string
	Ports []IngressPort
	// AdoptFrom names an owner whose public ports this one takes over, route
	// by route, matched on name. it is how a migrated environment keeps its
	// url. empty means fresh ports.
	AdoptFrom string
}

// IngressRoute is one published port.
type IngressRoute struct {
	Name      string
	GuestPort int
	// URL is the stable host:port clients connect to.
	URL string
}

// IngressGrant is what the proxy answers: where the guest should dial, how to
// recognise the proxy, and the public side of each route.
type IngressGrant struct {
	ProxyAddr     string
	ServerCertPEM string
	Routes        []IngressRoute
}

// IngressProxy is the orchestrator's view of fuse-proxy's admin api. the
// implementation lives in internal/ingress, which imports this package.
type IngressProxy interface {
	Publish(ctx context.Context, req IngressRequest) (IngressGrant, error)
	Lookup(ctx context.Context, owner string) (IngressGrant, error)
	Unpublish(ctx context.Context, owner string) error
}

// ingressPlan is a published vm: the file its guest needs and what to report.
type ingressPlan struct {
	tunnelConfig []byte
	grant        IngressGrant
	request      IngressRequest
}

// publishIngress publishes the agent port plus every tcp expose for vmID. it
// returns nil, nil when no proxy is configured.
func (fm *FleetManager) publishIngress(ctx context.Context, vmID string, expose []ExposeSpec, adoptFrom string) (*ingressPlan, error) {
	if fm.ingress == nil {
		return nil, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("mint tunnel token: %w", err)
	}
	token := hex.EncodeToString(raw)

	ports := []IngressPort{{Name: ingressAgentRoute, GuestPort: fusedGuestPort}}
	for _, ex := range expose {
		// udp stays on the host's dnat: a route is a tcp stream.
		if ex.Protocol == ProtocolUDP {
			continue
		}
		ports = append(ports, IngressPort{Name: ingressExposeName(ex), GuestPort: ex.Port})
	}

	request := IngressRequest{Owner: vmID, Token: token, Ports: ports, AdoptFrom: adoptFrom}
	grant, err := fm.ingress.Publish(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("publish ingress for %s: %w", vmID, err)
	}
	guestPorts := make([]int, 0, len(ports))
	for _, p := range ports {
		guestPorts = append(guestPorts, p.GuestPort)
	}
	config, err := json.Marshal(tunnel.Config{
		ProxyAddr:     grant.ProxyAddr,
		ServerCertPEM: grant.ServerCertPEM,
		Owner:         vmID,
		Token:         token,
		Ports:         guestPorts,
	})
	if err != nil {
		return nil, err
	}
	return &ingressPlan{tunnelConfig: config, grant: grant, request: request}, nil
}

// adoptIngress moves fromVMID's public ports onto plan's vm, which is what
// keeps a migrated environment's url. it is a second publish with the same
// token and ports, so the guest's tunnel.json stays valid and only the public
// side changes.
//
// it runs last, once the new vm is running and recorded, because it is the one
// step that takes something from the source: everything before it can fail and
// leave the source exactly as reachable as it was. a failure here is logged and
// not returned, since the migrate itself worked and the new vm has a url, just
// not the old one.
func (fm *FleetManager) adoptIngress(ctx context.Context, plan *ingressPlan, fromVMID string) {
	if plan == nil {
		return
	}
	request := plan.request
	request.AdoptFrom = fromVMID
	grant, err := fm.ingress.Publish(ctx, request)
	if err != nil {
		fm.logger.Warn("adopt ingress failed; the migrated vm keeps a new url", "vm", request.Owner, "from", fromVMID, "err", err)
		return
	}
	plan.grant = grant
}

// withTunnelFiles returns files plus p's guest file. files is not modified.
func (p *ingressPlan) withTunnelFiles(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte)
	for path, data := range files {
		out[path] = data
	}
	for path, data := range p.tunnelFiles() {
		out[path] = data
	}
	return out
}

// retunnelWithoutCredentials gives a cloned guest its own tunnel identity on a
// fleet with no encryption key, where fork and migrate otherwise leave the
// guest agent alone. the clone booted a copy of its source's disk, tunnel.json
// included, and the source's identity stops working the moment the source is
// destroyed, so skipping this would leave the clone with no ingress at all.
func (fm *FleetManager) retunnelWithoutCredentials(ctx context.Context, env Environment, plan *ingressPlan, drainCommand string) error {
	if plan == nil {
		return nil
	}
	if err := uploadFiles(ctx, env, plan.tunnelFiles()); err != nil {
		return err
	}
	return env.StartAgent(ctx, AgentSpec{DrainCommand: drainCommand, TunnelConfigPath: plan.tunnelConfigPath()})
}

// tunnelFiles is the guest file a published vm needs, or nothing.
func (p *ingressPlan) tunnelFiles() map[string][]byte {
	if p == nil {
		return nil
	}
	return map[string][]byte{TunnelConfigGuestPath: p.tunnelConfig}
}

// tunnelConfigPath is what AgentSpec.TunnelConfigPath should be for p.
func (p *ingressPlan) tunnelConfigPath() string {
	if p == nil {
		return ""
	}
	return TunnelConfigGuestPath
}

// ingressSupported reports whether a vm on hostID can run the tunnel sidecar.
// only the firecracker host agent starts one; a qemu (gpu) environment keeps
// its dnat url rather than being published to a port nothing will ever answer.
// an empty hostID is single-provider mode, where the provider is firecracker.
//
// an environment with proxy egress is left out too. its tap drops everything
// outbound that is not the egress proxy, and the sidecar's udp to fuse-proxy is
// outbound; publishing it would trade a working dnat url for a dead one.
func (fm *FleetManager) ingressSupported(hostID string, spec Spec) bool {
	if spec.Egress.IsProxy() {
		return false
	}
	if hostID == "" {
		return true
	}
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	h, ok := fm.hosts[hostID]
	return ok && (h.Backend == "" || h.Backend == BackendFirecracker)
}

func ingressExposeName(ex ExposeSpec) string {
	if ex.As != "" {
		return "expose-" + ex.As
	}
	return fmt.Sprintf("expose-%d", ex.Port)
}

// unpublishIngress is best effort: a route left behind points at a guest that
// will never dial in again, which costs a port until the next publish reuses
// the owner, and must never be the reason a destroy fails.
func (fm *FleetManager) unpublishIngress(ctx context.Context, vmID string) {
	if fm.ingress == nil {
		return
	}
	if err := fm.ingress.Unpublish(ctx, vmID); err != nil && !errors.Is(err, ErrIngressNotFound) {
		fm.logger.Warn("unpublish ingress failed", "vm", vmID, "err", err)
	}
}

// applyIngress rewrites what a vm reports so clients see the stable addresses.
// the dnat url stays on v.env, which is what the orchestrator itself keeps
// using to reach the guest: its own calls have no reason to depend on the
// proxy being up. caller holds fm.mu.
func (v *vm) applyIngress(grant IngressGrant) {
	byPort := make(map[int]string, len(grant.Routes))
	for _, r := range grant.Routes {
		if r.Name == ingressAgentRoute {
			v.publicURL = r.URL
			continue
		}
		byPort[r.GuestPort] = r.URL
	}
	for i, ep := range v.endpoints {
		if url, ok := byPort[ep.Port]; ok && ep.Protocol != ProtocolUDP {
			v.endpoints[i].URL = url
		}
	}
}
