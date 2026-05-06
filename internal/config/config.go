// Package config loads and persists tn's host configuration.
//
// Config lives at $TN_HOME (default ~/.tn) as config.yaml. Secrets are never
// written here: passwords come from a `passwordCmd` (a shell command whose
// stdout is the password — works with `pass`, `op read`, `security
// find-generic-password`, etc.) or from an interactive prompt.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Hop struct {
	// Addr is host:port. If port is omitted, 22 is assumed.
	Addr string `yaml:"addr"`
	User string `yaml:"user"`
	// IdentityFile is an optional path to a private key (PEM). Tilde expansion
	// is supported.
	IdentityFile string `yaml:"identityFile,omitempty"`
	// PasswordCmd, when non-empty, is executed with /bin/sh -c; its stdout
	// (trimmed) is used as the password. This avoids storing plaintext.
	PasswordCmd string `yaml:"passwordCmd,omitempty"`
}

type Host struct {
	Name string `yaml:"-"`
	// Target is the final SSH target (the machine we run commands on).
	Target Hop `yaml:"target"`
	// Jump, when set, is the bastion/jumpbox we tunnel through.
	Jump *Hop `yaml:"jump,omitempty"`
	// KnownHosts, when set, overrides the default $TN_HOME/known_hosts.
	KnownHosts string `yaml:"knownHosts,omitempty"`
}

type Config struct {
	DefaultHost string           `yaml:"defaultHost,omitempty"`
	Hosts       map[string]*Host `yaml:"hosts"`
}

// Home returns the directory storing config and runtime files.
func Home() string {
	if v := os.Getenv("TN_HOME"); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".tn"
	}
	return filepath.Join(h, ".tn")
}

func path() string { return filepath.Join(Home(), "config.yaml") }

// Load reads config from disk. A missing file returns an empty Config (not an
// error) so first-run flows can prompt for setup.
func Load() (*Config, error) {
	data, err := os.ReadFile(path())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Hosts: map[string]*Host{}}, nil
	}
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path(), err)
	}
	if c.Hosts == nil {
		c.Hosts = map[string]*Host{}
	}
	for name, h := range c.Hosts {
		h.Name = name
	}
	return c, nil
}

// Save writes config atomically with mode 0600.
func Save(c *Config) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path())
}

// Resolve returns the host with the given name. If name is empty, falls back
// to $TN_HOST then DefaultHost. If only one host is configured, that one is
// returned regardless.
func (c *Config) Resolve(name string) (*Host, error) {
	if name == "" {
		name = os.Getenv("TN_HOST")
	}
	if name == "" {
		name = c.DefaultHost
	}
	if name == "" && len(c.Hosts) == 1 {
		for _, h := range c.Hosts {
			return h, nil
		}
	}
	if name == "" {
		return nil, errors.New("no host specified: use --host, $TN_HOST, or set defaultHost in config")
	}
	h, ok := c.Hosts[name]
	if !ok {
		return nil, fmt.Errorf("unknown host %q (configured: %s)", name, strings.Join(c.HostNames(), ", "))
	}
	return h, nil
}

func (c *Config) HostNames() []string {
	out := make([]string, 0, len(c.Hosts))
	for k := range c.Hosts {
		out = append(out, k)
	}
	return out
}

// EnsureAddrPort appends :22 to addr if it has no port. Accepts plain IPv6
// addresses inside brackets.
func EnsureAddrPort(addr string) string {
	if addr == "" {
		return addr
	}
	if strings.HasPrefix(addr, "[") {
		if i := strings.LastIndex(addr, "]"); i != -1 && !strings.Contains(addr[i:], ":") {
			return addr + ":22"
		}
		return addr
	}
	if !strings.Contains(addr, ":") {
		return addr + ":22"
	}
	return addr
}

// ExpandPath expands a leading ~ to the user's home directory.
func ExpandPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return h
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(h, p[2:])
	}
	return p
}
