package cli

import (
	"strings"
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

func TestRTTStr(t *testing.T) {
	cases := map[int64]string{637: "637ms", 7173: "7.2s", 65000: "1m"}
	for ms, want := range cases {
		if got := rttStr(ms); got != want {
			t.Errorf("rttStr(%d) = %q, want %q", ms, got, want)
		}
	}
}

// humanKB takes KiB, because that is what /proc/meminfo and df -P report.
func TestHumanKB(t *testing.T) {
	cases := map[int64]string{
		512 * 1024:             "512M",
		1024 * 1024:            "1.0G",
		500 * 1024 * 1024:      "500G",
		2 * 1024 * 1024 * 1024: "2.0T",
	}
	for kb, want := range cases {
		if got := humanKB(kb); got != want {
			t.Errorf("humanKB(%d) = %q, want %q", kb, got, want)
		}
	}
}

func TestRenderStatusColorAndAbsolute(t *testing.T) {
	up := poolState{Name: "box", Addr: "10.0.0.1:22", Up: true, RTTMs: 7173,
		CPU: 45, Mem: 20, Disk: 80, GPU: -1,
		NCPU: 8, Load: 3.6,
		MemUsedKB: 10 * 1024 * 1024, MemTotalKB: 50 * 1024 * 1024,
		DiskUsedKB: 800 * 1024 * 1024, DiskTotalKB: 1000 * 1024 * 1024,
	}
	down := poolState{Name: "dead", Addr: "10.0.0.2:22",
		CPU: -1, Mem: -1, Disk: -1, GPU: -1, Detail: "connection refused"}

	out := renderPool([]poolState{up, down}, -1, 1, 120, true)
	for _, want := range []string{
		"\033[32m●", "\033[31m●", // green up, red down
		"7.2s",               // RTT scaled to seconds
		"45%",                // CPU is a percentage only
		"10G/50G",            // memory absolute
		"800G/1.0T",          // disk absolute, unit auto-scaled
		"connection refused", // failure reason, not blank columns
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "3.60/8") {
		t.Error("CPU should show the percentage only, not load/cores")
	}
}

func TestRenderNoColorWhenAsked(t *testing.T) {
	s := poolState{Name: "box", Addr: "10.0.0.1:22", Up: true, RTTMs: 12,
		CPU: -1, Mem: -1, Disk: -1, GPU: -1}
	out := renderPool([]poolState{s}, -1, 1, 120, false)
	if strings.Contains(out, "\033[") {
		t.Error("colour escapes emitted with color=false")
	}
	if !strings.Contains(out, "o ") {
		t.Error("plain mode should use an ASCII status glyph")
	}
}
