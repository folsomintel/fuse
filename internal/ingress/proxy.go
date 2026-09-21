// Package ingress is fuse-proxy: stable public tcp ports in front of guests
// that dial in over internal/tunnel.
//
// a route is a public port that belongs to an owner (a vm id) and leads to one
// port inside that owner's guest. the port is the stable thing. which owner it
// belongs to can change, and that is the whole of what a migrate does here:
// the new vm adopts the old vm's ports, and the url every client holds keeps
// working without any of them learning that anything moved.
package ingress

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/tunnel"
)

// Route is one published port.
type Route struct {
	Name       string `json:"name"`
	GuestPort  int    `json:"guest_port"`
	PublicPort int    `json:"public_port"`
}

// ownerState is what is remembered about one owner. only a hash of the token
// is kept: the state file is a list of credentials otherwise.
type ownerState struct {
	TokenSHA256 string  `json:"token_sha256"`
	Routes      []Route `json:"routes"`
}

// Opener is the part of tunnel.Server the proxy uses.
type Opener interface {
	Open(ctx context.Context, owner string, port int) (net.Conn, error)
	Drop(owner string)
}

// Config tunes a Proxy.
type Config struct {
	// StatePath is where owners and routes are persisted. routes have to
	// outlive the process: a proxy that came back with different ports would
	// have broken every url it exists to keep stable.
	StatePath string
	// PortMin and PortMax bound the public ports handed out, inclusive.
	PortMin, PortMax int
	// BindHost is the address public listeners bind, "" for all.
	BindHost string
	// HoldTimeout is how long a client connection waits for its guest to be
	// reachable before it is closed.
	HoldTimeout time.Duration
	Logger      *slog.Logger
}

// Proxy owns the routes and their listeners.
type Proxy struct {
	cfg    Config
	tunnel Opener

	mu        sync.Mutex
	owners    map[string]*ownerState
	listeners map[int]net.Listener
	// target is the accept path's view of owners: public port to where it
	// leads. kept alongside owners so an accept is one map read.
	target map[int]routeTarget
}

type routeTarget struct {
	owner     string
	guestPort int
}

// New loads the state file and reopens every route's listener.
func New(cfg Config) (*Proxy, error) {
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = 30 * time.Second
	}
	if cfg.PortMin <= 0 || cfg.PortMax < cfg.PortMin || cfg.PortMax > 65535 {
		return nil, fmt.Errorf("ingress: invalid port range %d-%d", cfg.PortMin, cfg.PortMax)
	}
	p := &Proxy{
		cfg:       cfg,
		owners:    make(map[string]*ownerState),
		listeners: make(map[int]net.Listener),
		target:    make(map[int]routeTarget),
	}
	raw, err := os.ReadFile(cfg.StatePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &p.owners); err != nil {
			return nil, fmt.Errorf("ingress: read %s: %w", cfg.StatePath, err)
		}
	}
	return p, nil
}

// Start attaches the tunnel and opens the listeners for every known route. it
// is separate from New because the tunnel server's authenticator is this
// proxy's Authenticate, so each needs the other to exist first.
func (p *Proxy) Start(t Opener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tunnel = t
	for owner, st := range p.owners {
		for _, r := range st.Routes {
			if err := p.listenLocked(r.PublicPort); err != nil {
				// the port is still the route's: a client gets a refused
				// connection until whatever took it lets go, rather than the
				// url silently changing.
				p.cfg.Logger.Error("reopen route failed", "owner", owner, "port", r.PublicPort, "err", err)
			}
			p.target[r.PublicPort] = routeTarget{owner: owner, guestPort: r.GuestPort}
		}
	}
}

// Close stops every listener.
func (p *Proxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port, ln := range p.listeners {
		_ = ln.Close()
		delete(p.listeners, port)
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authenticate is the tunnel server's authenticator.
func (p *Proxy) Authenticate(owner, token string) bool {
	p.mu.Lock()
	st := p.owners[owner]
	p.mu.Unlock()
	if st == nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(st.TokenSHA256), []byte(hashToken(token))) == 1
}

// PortSpec is one port of a Publish.
type PortSpec struct {
	Name      string `json:"name"`
	GuestPort int    `json:"guest_port"`
}

// Publish creates or updates owner so that it has exactly ports, and returns
// its routes.
//
// a route that already exists under the same name keeps its public port, so
// publishing again is how a token is rotated without the url moving. with
// adoptFrom, each name takes the public port of the same name under adoptFrom,
// and adoptFrom loses it. connections already
// running to the old guest are left alone: they are streams on the old guest's
// tunnel and end when it does.
func (p *Proxy) Publish(owner, token string, ports []PortSpec, adoptFrom string) ([]Route, error) {
	if owner == "" || token == "" {
		return nil, errors.New("owner and token are required")
	}
	seen := make(map[string]bool, len(ports))
	for _, ps := range ports {
		if ps.Name == "" || ps.GuestPort < 1 || ps.GuestPort > 65535 || seen[ps.Name] {
			return nil, fmt.Errorf("invalid or duplicate port %q (%d)", ps.Name, ps.GuestPort)
		}
		seen[ps.Name] = true
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.owners[owner]
	if st == nil {
		st = &ownerState{}
	}
	tokenChanged := st.TokenSHA256 != "" && st.TokenSHA256 != hashToken(token)
	have := make(map[string]Route, len(st.Routes))
	for _, r := range st.Routes {
		have[r.Name] = r
	}
	// the donor is edited as a copy and written back only once every port has
	// been found, so a publish that fails half way has taken nothing from it.
	var donor *ownerState
	var donorRoutes []Route
	if adoptFrom != owner {
		if donor = p.owners[adoptFrom]; donor != nil {
			donorRoutes = append([]Route(nil), donor.Routes...)
		}
	}

	routes := make([]Route, 0, len(ports))
	var allocated []int
	for _, ps := range ports {
		// the donor's port wins over one the owner already has under the same
		// name. a migrate publishes the new vm first, so its guest can dial in
		// and be checked, and adopts only once it is known good; by then the
		// new vm has a port of its own, and it is the donor's that has to
		// survive. the owner's is left in have and closed below.
		r, ok := takeRoute(&donorRoutes, ps.Name)
		if !ok {
			r, ok = have[ps.Name]
			delete(have, ps.Name)
		}
		if !ok {
			port, err := p.allocateLocked()
			if err != nil {
				for _, port := range allocated {
					p.closeLocked(port)
				}
				return nil, err
			}
			allocated = append(allocated, port)
			// reserved at once, so the next allocation in this loop skips it.
			p.target[port] = routeTarget{owner: owner, guestPort: ps.GuestPort}
			r = Route{PublicPort: port}
		}
		r.Name, r.GuestPort = ps.Name, ps.GuestPort
		routes = append(routes, r)
	}
	// whatever is left was published before and is not wanted now.
	for _, r := range have {
		p.closeLocked(r.PublicPort)
	}

	if donor != nil {
		donor.Routes = donorRoutes
	}
	st.TokenSHA256 = hashToken(token)
	st.Routes = routes
	p.owners[owner] = st
	for _, r := range routes {
		p.target[r.PublicPort] = routeTarget{owner: owner, guestPort: r.GuestPort}
	}
	if err := p.saveLocked(); err != nil {
		return nil, err
	}
	if tokenChanged && p.tunnel != nil {
		// a guest still connected under the old token is not the guest this
		// owner now means.
		go p.tunnel.Drop(owner)
	}
	return append([]Route(nil), routes...), nil
}

func takeRoute(routes *[]Route, name string) (Route, bool) {
	for i, r := range *routes {
		if r.Name == name {
			*routes = append((*routes)[:i], (*routes)[i+1:]...)
			return r, true
		}
	}
	return Route{}, false
}

// Routes returns owner's routes.
func (p *Proxy) Routes(owner string) ([]Route, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.owners[owner]
	if st == nil {
		return nil, false
	}
	return append([]Route(nil), st.Routes...), true
}

// Remove deletes owner, closes its ports and disconnects its guest.
func (p *Proxy) Remove(owner string) (bool, error) {
	p.mu.Lock()
	st := p.owners[owner]
	if st == nil {
		p.mu.Unlock()
		return false, nil
	}
	for _, r := range st.Routes {
		p.closeLocked(r.PublicPort)
	}
	delete(p.owners, owner)
	err := p.saveLocked()
	t := p.tunnel
	p.mu.Unlock()
	if t != nil {
		t.Drop(owner)
	}
	return true, err
}

// allocateLocked finds a free public port and starts listening on it. binding
// is the test: a port in the range that something else on the machine holds is
// skipped rather than handed out.
func (p *Proxy) allocateLocked() (int, error) {
	for port := p.cfg.PortMin; port <= p.cfg.PortMax; port++ {
		if _, used := p.target[port]; used {
			continue
		}
		if err := p.listenLocked(port); err != nil {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free public port in %d-%d", p.cfg.PortMin, p.cfg.PortMax)
}

func (p *Proxy) listenLocked(port int) error {
	if _, ok := p.listeners[port]; ok {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(p.cfg.BindHost, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	p.listeners[port] = ln
	go p.accept(ln, port)
	return nil
}

func (p *Proxy) closeLocked(port int) {
	if ln, ok := p.listeners[port]; ok {
		_ = ln.Close()
		delete(p.listeners, port)
	}
	delete(p.target, port)
}

func (p *Proxy) accept(ln net.Listener, port int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go p.serve(conn, port)
	}
}

// serve connects one client to whichever guest the port leads to right now.
// the lookup happens per connection, so adopting a port needs no listener
// swap and cannot drop a connection that is mid-accept.
func (p *Proxy) serve(client net.Conn, port int) {
	p.mu.Lock()
	target, ok := p.target[port]
	t := p.tunnel
	p.mu.Unlock()
	if !ok || t == nil {
		_ = client.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.HoldTimeout)
	guest, err := t.Open(ctx, target.owner, target.guestPort)
	cancel()
	if err != nil {
		p.cfg.Logger.Debug("no guest for connection", "owner", target.owner, "port", port, "err", err)
		_ = client.Close()
		return
	}
	tunnel.Pipe(client, guest)
}

// saveLocked writes the state file atomically.
func (p *Proxy) saveLocked() error {
	for _, st := range p.owners {
		sort.Slice(st.Routes, func(i, j int) bool { return st.Routes[i].Name < st.Routes[j].Name })
	}
	raw, err := json.MarshalIndent(p.owners, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.cfg.StatePath), ".routes-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.cfg.StatePath)
}
