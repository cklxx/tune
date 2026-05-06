package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cklxx/tune/internal/config"
	"golang.org/x/crypto/ssh"
)

// Two in-process SSH servers — one playing the jumpbox, one the target — let
// us exercise the full dial-through-jump path without docker/sshd.
//
// The target server supports an "exec" session that echoes the requested
// command back as stdout (and exits 0). The jump server handles "direct-tcpip"
// channels by bridging them to the target's listener address.

type testServer struct {
	addr   string
	pubKey ssh.PublicKey
	stop   func()
}

// startTarget runs a minimal SSH server that handles "session" channels and
// the "exec" request, replying with "EXEC: <cmd>" on stdout.
func startTarget(t *testing.T, allowed ssh.PublicKey) *testServer {
	t.Helper()
	hostKey, hostPub := genKey(t)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(allowed.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("not allowed")
		},
	}
	cfg.AddHostKey(hostKey)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serveTarget(c, cfg)
		}
	}()
	return &testServer{addr: l.Addr().String(), pubKey: hostPub, stop: func() { l.Close() }}
}

func serveTarget(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			continue
		}
		go func(ch ssh.Channel, in <-chan *ssh.Request) {
			defer ch.Close()
			for r := range in {
				switch r.Type {
				case "exec":
					var p struct{ Command string }
					_ = ssh.Unmarshal(r.Payload, &p)
					r.Reply(true, nil)
					_, _ = io.WriteString(ch, "EXEC: "+p.Command+"\n")
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				default:
					if r.WantReply {
						r.Reply(false, nil)
					}
				}
			}
		}(ch, in)
	}
}

// startJump runs an SSH server that bridges "direct-tcpip" channels to
// arbitrary destinations.
func startJump(t *testing.T, allowed ssh.PublicKey) *testServer {
	t.Helper()
	hostKey, hostPub := genKey(t)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(allowed.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("not allowed")
		},
	}
	cfg.AddHostKey(hostKey)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serveJump(c, cfg)
		}
	}()
	return &testServer{addr: l.Addr().String(), pubKey: hostPub, stop: func() { l.Close() }}
}

func serveJump(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			nc.Reject(ssh.UnknownChannelType, "jump only")
			continue
		}
		var p struct {
			Host       string
			Port       uint32
			Originator string
			OrigPort   uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
			nc.Reject(ssh.ConnectionFailed, "bad payload")
			continue
		}
		dest := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
		out, err := net.DialTimeout("tcp", dest, 5*time.Second)
		if err != nil {
			nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			out.Close()
			continue
		}
		go ssh.DiscardRequests(reqs)
		go func() { defer ch.Close(); defer out.Close(); io.Copy(ch, out) }()
		go func() { defer ch.Close(); defer out.Close(); io.Copy(out, ch) }()
	}
}

// genKey returns an ed25519 ssh.Signer and its public key.
func genKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, signer.PublicKey()
}

// genKeyPair returns a signer plus a path to its on-disk private key.
func genKeyPair(t *testing.T) (ssh.Signer, ssh.PublicKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "id")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return signer, signer.PublicKey(), p
}

// TestDialDirect verifies a direct (no-jump) dial + exec round trip.
func TestDialDirect(t *testing.T) {
	clientSigner, _, keyPath := genKeyPair(t)

	srv := startTarget(t, clientSigner.PublicKey())
	defer srv.stop()

	t.Setenv("TN_HOME", t.TempDir())

	host := &config.Host{
		Name:       "test",
		Target:     config.Hop{Addr: srv.addr, User: "alice", IdentityFile: keyPath},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	// Bypass TOFU prompt by using PolicyInsecure.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, host, PolicyInsecure)
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
}

// TestDialThroughJump verifies the jumpbox path: client -> jump -> target.
func TestDialThroughJump(t *testing.T) {
	clientSigner, _, keyPath := genKeyPair(t)

	target := startTarget(t, clientSigner.PublicKey())
	defer target.stop()

	jump := startJump(t, clientSigner.PublicKey())
	defer jump.stop()

	t.Setenv("TN_HOME", t.TempDir())

	host := &config.Host{
		Name:       "test",
		Jump:       &config.Hop{Addr: jump.addr, User: "alice", IdentityFile: keyPath},
		Target:     config.Hop{Addr: target.addr, User: "alice", IdentityFile: keyPath},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, host, PolicyInsecure)
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

	// Ping should also work after jump dial.
	if _, err := c.Ping(); err != nil {
		t.Fatalf("ping after jump: %v", err)
	}
}
