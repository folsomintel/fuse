// package config manages the local cli state: orchestrator contexts (base url
// + token) and the per-context active host. it is persisted to a yaml file
// under the user's config dir. it knows nothing about the api.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ErrNoContext is returned when an operation needs an active context but none
// is selected.
var ErrNoContext = errors.New("no active context: run `fuse connect <url> --token <token>` first")

// Context is a single orchestrator the cli can talk to. one orchestrator is
// one control plane (base url + bearer token). hosts live inside it.
type Context struct {
	Name string `yaml:"name"`
	// BaseURL is the orchestrator root (scheme://host:port), no /v1 suffix.
	BaseURL string `yaml:"base_url"`
	// Token is the bearer token. empty is allowed for dev/insecure mode.
	Token string `yaml:"token,omitempty"`
	// Master marks a context whose token is the master token (required for
	// api-key commands). informational only; the server is the source of truth.
	Master bool `yaml:"master,omitempty"`
	// ActiveHost is the host id selected via `fuse host <id>`. it scopes the
	// host-scoped commands for this context.
	ActiveHost string `yaml:"active_host,omitempty"`
}

// Config is the whole cli state.
type Config struct {
	CurrentContext string    `yaml:"current_context,omitempty"`
	Contexts       []Context `yaml:"contexts,omitempty"`

	// path is where this config was loaded from / will be saved to. not
	// serialized.
	path string `yaml:"-"`
}

// DefaultPath returns the config file path, honoring XDG_CONFIG_HOME and
// falling back to ~/.config/fuse/config.yaml.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "fuse", "config.yaml"), nil
}

// Load reads the config from path. if path is empty, DefaultPath is used. a
// missing file is not an error: an empty config (with path set) is returned so
// the caller can add a context and Save.
func Load(path string) (*Config, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg := &Config{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path
	return cfg, nil
}

// Path returns the file this config is bound to.
func (c *Config) Path() string { return c.path }

// Save writes the config to its path with restrictive permissions (it holds
// tokens). the parent directory is created if needed.
func (c *Config) Save() error {
	if c.path == "" {
		p, err := DefaultPath()
		if err != nil {
			return err
		}
		c.path = p
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(c.path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// find returns a pointer to the context with the given name, or nil. the
// pointer is into the backing slice so mutations persist.
func (c *Config) find(name string) *Context {
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			return &c.Contexts[i]
		}
	}
	return nil
}

// Get returns the named context or an error.
func (c *Config) Get(name string) (*Context, error) {
	if ctx := c.find(name); ctx != nil {
		return ctx, nil
	}
	return nil, fmt.Errorf("context %q not found", name)
}

// Current returns the active context. if name is non-empty it overrides the
// stored current context (used for a per-invocation --context flag).
func (c *Config) Current(override string) (*Context, error) {
	name := override
	if name == "" {
		name = c.CurrentContext
	}
	if name == "" {
		return nil, ErrNoContext
	}
	ctx := c.find(name)
	if ctx == nil {
		return nil, fmt.Errorf("context %q not found", name)
	}
	return ctx, nil
}

// Add inserts or replaces a context by name and makes it current.
func (c *Config) Add(ctx Context) {
	if existing := c.find(ctx.Name); existing != nil {
		// preserve the active host selection across re-connects.
		if ctx.ActiveHost == "" {
			ctx.ActiveHost = existing.ActiveHost
		}
		*existing = ctx
	} else {
		c.Contexts = append(c.Contexts, ctx)
	}
	c.CurrentContext = ctx.Name
}

// Use sets the current context, validating it exists.
func (c *Config) Use(name string) error {
	if c.find(name) == nil {
		return fmt.Errorf("context %q not found", name)
	}
	c.CurrentContext = name
	return nil
}

// Remove deletes a context. if it was current, the current selection is
// cleared.
func (c *Config) Remove(name string) error {
	idx := -1
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("context %q not found", name)
	}
	c.Contexts = append(c.Contexts[:idx], c.Contexts[idx+1:]...)
	if c.CurrentContext == name {
		c.CurrentContext = ""
	}
	return nil
}

// SetActiveHost records the selected host for the current context.
func (c *Config) SetActiveHost(override, hostID string) error {
	ctx, err := c.Current(override)
	if err != nil {
		return err
	}
	ctx.ActiveHost = hostID
	return nil
}
