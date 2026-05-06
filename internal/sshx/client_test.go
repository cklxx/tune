package sshx_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshtest"
	"github.com/cklxx/tune/internal/sshx"
)

// TestDialDirect verifies a direct (no-jump) dial + exec round trip.
func TestDialDirect(t *testing.T) {
	kp := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{
		AllowedKey: kp.PublicKey,
		AllowExec:  true,
	})

	host := &config.Host{
		Name:       "test",
		Target:     config.Hop{Addr: srv.Addr, User: "alice", IdentityFile: kp.Path},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sshx.Dial(ctx, host, sshx.PolicyInsecure)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	sess, err := c.SSH().NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("uname -a")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.HasPrefix(string(out), "EXEC: uname -a") {
		t.Fatalf("unexpected exec output: %q", out)
	}

	if rtt, err := c.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	} else if rtt < 0 || rtt > 5*time.Second {
		t.Fatalf("implausible RTT: %v", rtt)
	}
}

// TestDialThroughJump verifies the jumpbox path: client -> jump -> target.
func TestDialThroughJump(t *testing.T) {
	kp := sshtest.GenKey(t)
	target := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowExec: true})
	jump := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowDirect: true})

	host := &config.Host{
		Name:       "test",
		Jump:       &config.Hop{Addr: jump.Addr, User: "alice", IdentityFile: kp.Path},
		Target:     config.Hop{Addr: target.Addr, User: "alice", IdentityFile: kp.Path},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sshx.Dial(ctx, host, sshx.PolicyInsecure)
	if err != nil {
		t.Fatalf("dial via jump: %v", err)
	}
	defer c.Close()

	sess, err := c.SSH().NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("hi")
	if err != nil {
		t.Fatalf("exec via jump: %v", err)
	}
	if !strings.HasPrefix(string(out), "EXEC: hi") {
		t.Fatalf("unexpected exec output via jump: %q", out)
	}

	if _, err := c.Ping(); err != nil {
		t.Fatalf("ping after jump: %v", err)
	}
}

// TestDialRejectsBadKey ensures auth failure surfaces clearly.
func TestDialRejectsBadKey(t *testing.T) {
	good := sshtest.GenKey(t)
	wrong := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{AllowedKey: good.PublicKey, AllowExec: true})

	// Bypass the password prompt: we don't want to hang waiting for stdin.
	prevPrompt := sshx.PasswordPrompt
	t.Cleanup(func() { sshx.PasswordPrompt = prevPrompt })
	sshx.PasswordPrompt = func(string) (string, error) { return "wrong", nil }

	host := &config.Host{
		Name:       "test",
		Target:     config.Hop{Addr: srv.Addr, User: "bob", IdentityFile: wrong.Path},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := sshx.Dial(ctx, host, sshx.PolicyInsecure); err == nil {
		t.Fatal("expected dial with wrong key to fail")
	}
}
