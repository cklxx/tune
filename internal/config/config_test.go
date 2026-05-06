package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TN_HOME", tmp)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(cfg.Hosts) != 0 {
		t.Fatalf("expected empty hosts on first load")
	}

	cfg.DefaultHost = "dev"
	cfg.Hosts["dev"] = &Host{
		Target: Hop{Addr: "10.0.0.1", User: "alice", IdentityFile: "~/.ssh/id_rsa"},
		Jump:   &Hop{Addr: "jump.example.com:2222", User: "alice", PasswordCmd: "echo hunter2"},
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// File should be 0600.
	st, err := os.Stat(filepath.Join(tmp, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("config perms = %o, want 0600", got)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.DefaultHost != "dev" {
		t.Fatalf("defaultHost = %q", got.DefaultHost)
	}
	h := got.Hosts["dev"]
	if h == nil {
		t.Fatal("dev missing")
	}
	if h.Name != "dev" {
		t.Fatalf("Name not populated: %q", h.Name)
	}
	if h.Jump == nil || h.Jump.Addr != "jump.example.com:2222" {
		t.Fatalf("jump not preserved: %+v", h.Jump)
	}
}

func TestResolve(t *testing.T) {
	cfg := &Config{Hosts: map[string]*Host{"dev": {Name: "dev"}, "stage": {Name: "stage"}}}

	// Multiple hosts without default and no flag/env: should error.
	if _, err := cfg.Resolve(""); err == nil {
		t.Fatal("expected error with multiple hosts and no default")
	}

	cfg.DefaultHost = "dev"
	h, err := cfg.Resolve("")
	if err != nil || h.Name != "dev" {
		t.Fatalf("Resolve('') = %v %v", h, err)
	}

	t.Setenv("TN_HOST", "stage")
	h, err = cfg.Resolve("")
	if err != nil || h.Name != "stage" {
		t.Fatalf("Resolve via env = %v %v", h, err)
	}

	h, err = cfg.Resolve("dev")
	if err != nil || h.Name != "dev" {
		t.Fatalf("Resolve('dev') = %v %v", h, err)
	}

	if _, err := cfg.Resolve("ghost"); err == nil {
		t.Fatal("expected error for unknown host")
	}

	// Single host w/ no default returns that one.
	cfg = &Config{Hosts: map[string]*Host{"only": {Name: "only"}}}
	t.Setenv("TN_HOST", "")
	h, err = cfg.Resolve("")
	if err != nil || h.Name != "only" {
		t.Fatalf("single-host fallthrough: %v %v", h, err)
	}
}

func TestEnsureAddrPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"host", "host:22"},
		{"host:2222", "host:2222"},
		{"[::1]", "[::1]:22"},
		{"[::1]:22", "[::1]:22"},
	}
	for _, c := range cases {
		if got := EnsureAddrPort(c.in); got != c.want {
			t.Errorf("EnsureAddrPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := ExpandPath("~/x"); got != filepath.Join(home, "x") {
		t.Errorf("expand ~/x = %q", got)
	}
	if got := ExpandPath(""); got != "" {
		t.Errorf("expand empty = %q", got)
	}
	if got := ExpandPath("/abs"); got != "/abs" {
		t.Errorf("expand /abs = %q", got)
	}
}
