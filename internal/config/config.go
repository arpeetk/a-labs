// Package config loads and persists the wren CLI's local configuration
// (~/.config/wren/config.yaml): the set of control-plane contexts and which one
// is current.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ErrNoContext is returned when no context is configured or selected.
var ErrNoContext = errors.New("no control plane configured — run `wren login` first")

// Context is a single control-plane target and its credentials.
type Context struct {
	Name   string `yaml:"name"`
	Server string `yaml:"server"`
	Org    string `yaml:"org,omitempty"`
	User   string `yaml:"user,omitempty"`
	Token  string `yaml:"token,omitempty"`
}

// Config is the on-disk CLI configuration.
type Config struct {
	CurrentContext string    `yaml:"currentContext,omitempty"`
	Contexts       []Context `yaml:"contexts,omitempty"`
}

// Dir returns the configuration directory, honoring WREN_CONFIG_DIR and
// XDG_CONFIG_HOME before falling back to ~/.config/wren.
func Dir() string {
	if d := os.Getenv("WREN_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "wren")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "wren")
}

// Path returns the config file path.
func Path() string { return filepath.Join(Dir(), "config.yaml") }

// Load reads the config, returning an empty config if the file is absent.
func Load() (*Config, error) {
	b, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(), err)
	}
	return &c, nil
}

// Save writes the config with owner-only permissions (it may hold tokens).
func (c *Config) Save() error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), b, 0o600)
}

// Resolve returns the named context, or the current context when name is empty.
func (c *Config) Resolve(name string) (*Context, error) {
	if name == "" {
		name = c.CurrentContext
	}
	if name == "" {
		return nil, ErrNoContext
	}
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			return &c.Contexts[i], nil
		}
	}
	return nil, fmt.Errorf("context %q not found in %s", name, Path())
}

// Upsert inserts or replaces a context by name.
func (c *Config) Upsert(ctx Context) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == ctx.Name {
			c.Contexts[i] = ctx
			return
		}
	}
	c.Contexts = append(c.Contexts, ctx)
}
