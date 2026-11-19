package cli

import (
	"testing"

	"github.com/cklxx/tune/internal/config"
)

func TestSSHDest(t *testing.T) {
	cases := []struct {
		name string
		host config.Host
		dest string
		port string
	}{
		{"implicit port", config.Host{Target: config.Hop{Addr: "10.0.0.1", User: "u"}}, "u@10.0.0.1", "22"},
		{"explicit port", config.Host{Target: config.Hop{Addr: "10.0.0.1:2222", User: "u"}}, "u@10.0.0.1", "2222"},
		{"ipv6", config.Host{Target: config.Hop{Addr: "[::1]:22", User: "u"}}, "u@::1", "22"},
	}
	for _, c := range cases {
		dest, port := sshDest(c.host)
		if dest != c.dest || port != c.port {
			t.Errorf("%s: got %q/%q want %q/%q", c.name, dest, port, c.dest, c.port)
		}
	}
}

func TestSSHArgsJump(t *testing.T) {
	h := config.Host{
		Target: config.Hop{Addr: "10.0.0.9:22", User: "tiger"},
		Jump:   &config.Hop{Addr: "jump:22", User: "me"},
	}
	args := sshArgs(h)
	if got := args[len(args)-3]; got != "-J" {
		t.Fatalf("missing -J: %v", args)
	}
	if got := args[len(args)-2]; got != "me@jump:22" {
		t.Fatalf("-J target = %q", got)
	}
}

func TestHasTag(t *testing.T) {
	h := config.Host{Tags: []string{"gpu", "bastion-only"}}
	if !hasTag(h, "bastion-only") || hasTag(h, "cpu") {
		t.Fatal("tag lookup wrong")
	}
}
