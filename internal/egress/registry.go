package egress

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{providers: map[string]Provider{}}
	for _, p := range providers {
		r.Register(p)
	}
	return r
}
func (r *Registry) Register(p Provider) {
	r.providers[p.Name()] = p
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Lookup(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

type UnknownProviderError struct {
	Name  string
	Known []string
}

func (e *UnknownProviderError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("egress: unknown provider %q (no egress providers are registered)", e.Name)
	}
	return fmt.Sprintf("egress: unknown provider %q (registered: %s)", e.Name, strings.Join(e.Known, ", "))
}

// ProvisionError is a backend's refusal or failure to come up, wrapped so a
// caller can tell an egress failure apart from every other boot failure and
// name the provider in what it reports.
type ProvisionError struct {
	Provider string
	VMID     string
	Err      error
}

func (e *ProvisionError) Error() string {
	return fmt.Sprintf("egress: provision %s for %s: %v", e.Provider, e.VMID, e.Err)
}

func (e *ProvisionError) Unwrap() error { return e.Err }

func (r *Registry) Provision(ctx context.Context, spec Spec, ec Context) (Endpoint, error) {
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		return Endpoint{}, err
	}
	if !spec.IsProxy() {
		return Endpoint{}, fmt.Errorf("egress: nothing to provision for mode %q", spec.Mode)
	}
	p, ok := r.Lookup(spec.Provider)
	if !ok {
		return Endpoint{}, &UnknownProviderError{Name: spec.Provider, Known: r.Names()}
	}
	ec.Protocol = spec.Protocol
	ep, err := p.Provision(ctx, ec)
	if err != nil {
		return Endpoint{}, &ProvisionError{Provider: spec.Provider, VMID: ec.VMID, Err: err}
	}
	if ep.Provider == "" {
		ep.Provider = spec.Provider
	}
	if ep.Protocol == "" {
		ep.Protocol = spec.Protocol
	}
	if ep.URL == "" {
		_ = p.Destroy(ctx, ec)
		return Endpoint{}, fmt.Errorf("egress: provider %s returned no endpoint for %s", spec.Provider, ec.VMID)
	}
	if ep.Protocol != spec.Protocol {
		_ = p.Destroy(ctx, ec)
		return Endpoint{}, fmt.Errorf("egress: provider %s answered a %s request with a %s endpoint for %s", spec.Provider, spec.Protocol, ep.Protocol, ec.VMID)
	}
	return ep, nil
}

func (r *Registry) Healthcheck(ctx context.Context, ep Endpoint) error {
	p, ok := r.Lookup(ep.Provider)
	if !ok {
		return &UnknownProviderError{Name: ep.Provider, Known: r.Names()}
	}
	return p.Healthcheck(ctx, ep)
}

// Destroy releases whatever provider holds for the vm described by ec. an
// unregistered provider is a no-op, not an error: teardown runs on paths
// that must not fail (reconcile cleanup, orphan reaping), and a provider
// that is no longer configured has nothing left to release here.
func (r *Registry) Destroy(ctx context.Context, provider string, ec Context) error {
	p, ok := r.Lookup(provider)
	if !ok {
		return nil
	}
	return p.Destroy(ctx, ec)
}

// Release asks every registered provider to destroy whatever it holds for
// the vm described by ec. it is for teardown paths that have no record of
// which provider was used, such as reconcile cleaning up an orphaned vm.
// every provider's Destroy is idempotent and tolerates a vm it never saw,
// so asking all of them is safe. the first error is returned after every
// provider has been asked.
func (r *Registry) Release(ctx context.Context, ec Context) error {
	var first error
	for _, name := range r.Names() {
		if err := r.providers[name].Destroy(ctx, ec); err != nil && first == nil {
			first = fmt.Errorf("egress: release %s for %s: %w", name, ec.VMID, err)
		}
	}
	return first
}
