// Package sshtest spins up minimal in-process SSH servers for tests.
//
// One server can play any combination of roles via Options:
//
//   - exec session ("EXEC: <cmd>" by default; ExecHandler to override)
//   - pty + shell (echoes "shell\n" then exits)
//   - sftp subsystem (full pkg/sftp server, optionally chrooted to SFTPRoot)
//   - direct-tcpip (jumpbox role: client opens channel with destination,
//     server bridges to a TCP dial)
//   - tcpip-forward (remote port-forward role: server listens on a local
//     address and opens a forwarded-tcpip channel back to the client per
//     accepted connection — this is what reverse SOCKS5 needs)
package sshtest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// KeyPair is an ed25519 SSH key generated for a test, with the OpenSSH-format
// private key persisted at Path so it can be fed to sshx.Dial via IdentityFile.
type KeyPair struct {
	Signer    ssh.Signer
	PublicKey ssh.PublicKey
	Path      string
}

// GenKey returns a new keypair, writing the private key under t.TempDir().
func GenKey(t testing.TB) KeyPair {
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
	return KeyPair{Signer: signer, PublicKey: signer.PublicKey(), Path: p}
}

// Options selects the capabilities of a test server.
type Options struct {
	// AllowedKey is the only public key that authenticates. If nil, any key
	// is accepted (rare in tests; prefer pinning).
	AllowedKey ssh.PublicKey

	AllowExec    bool
	AllowSFTP    bool
	AllowPTY     bool
	AllowDirect  bool // jumpbox role
	AllowForward bool // remote port forwarding (reverse SOCKS, etc.)

	// SFTPRoot, when set, is the working directory the SFTP server resolves
	// relative paths against.
	SFTPRoot string

	// ExecHandler is called for "exec" requests when AllowExec is true. It
	// returns the desired exit code. ch is the SSH channel for stdio. If nil,
	// a default handler echoes "EXEC: <cmd>\n" and exits 0.
	ExecHandler func(cmd string, ch ssh.Channel) int
}

// Server is a running test SSH server. Close it (or rely on t.Cleanup).
type Server struct {
	Addr     string
	HostKey  ssh.PublicKey
	listener net.Listener
	opts     Options
	wg       sync.WaitGroup
	closeMu  sync.Mutex
	closed   bool
}

// Close stops accepting new connections and waits for active ones to finish.
func (s *Server) Close() error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	s.closeMu.Unlock()
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

// Start brings up an SSH server with the given Options on 127.0.0.1:0.
// t.Cleanup is registered to close the server.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	hostSigner := generateHostKey(t)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if opts.AllowedKey == nil || bytes.Equal(k.Marshal(), opts.AllowedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("not allowed")
		},
	}
	cfg.AddHostKey(hostSigner)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: l.Addr().String(), HostKey: hostSigner.PublicKey(), listener: l, opts: opts}
	t.Cleanup(func() { _ = s.Close() })

	s.wg.Add(1)
	go s.acceptLoop(cfg)
	return s
}

func generateHostKey(t testing.TB) ssh.Signer {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func (s *Server) acceptLoop(cfg *ssh.ServerConfig) {
	defer s.wg.Done()
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.serve(c, cfg)
		}(c)
	}
}

func (s *Server) serve(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()

	// Per-connection state for tcpip-forward listeners: when the SSH conn
	// goes away (or Close is called), every listener it opened must close
	// too — otherwise the server's WaitGroup blocks forever in Close.
	cs := &connState{}
	defer cs.closeAll()

	go s.handleGlobalRequests(conn, reqs, cs)

	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			if !(s.opts.AllowExec || s.opts.AllowSFTP || s.opts.AllowPTY) {
				_ = nc.Reject(ssh.UnknownChannelType, "session disabled")
				continue
			}
			ch, in, err := nc.Accept()
			if err != nil {
				continue
			}
			go s.handleSession(ch, in)
		case "direct-tcpip":
			if !s.opts.AllowDirect {
				_ = nc.Reject(ssh.UnknownChannelType, "jump disabled")
				continue
			}
			go s.handleDirectTCPIP(nc)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// connState tracks per-SSH-connection resources that need cleanup when the
// connection ends.
type connState struct {
	mu        sync.Mutex
	forwards  []net.Listener
	closedAll bool
}

func (cs *connState) addForward(l net.Listener) (added bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closedAll {
		return false
	}
	cs.forwards = append(cs.forwards, l)
	return true
}

func (cs *connState) closeAll() {
	cs.mu.Lock()
	cs.closedAll = true
	fs := cs.forwards
	cs.forwards = nil
	cs.mu.Unlock()
	for _, l := range fs {
		_ = l.Close()
	}
}

// handleGlobalRequests responds to keepalives and (if enabled) tcpip-forward.
// Forward listeners are tied to cs so they're cleaned up when the SSH
// connection ends.
func (s *Server) handleGlobalRequests(conn *ssh.ServerConn, reqs <-chan *ssh.Request, cs *connState) {
	for r := range reqs {
		switch r.Type {
		case "tcpip-forward":
			if !s.opts.AllowForward {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			var p struct {
				Host string
				Port uint32
			}
			if err := ssh.Unmarshal(r.Payload, &p); err != nil {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			addr := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
			fl, err := net.Listen("tcp", addr)
			if err != nil {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			if !cs.addForward(fl) {
				_ = fl.Close()
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			boundPort := uint32(fl.Addr().(*net.TCPAddr).Port)
			if r.WantReply {
				_ = r.Reply(true, ssh.Marshal(struct{ Port uint32 }{boundPort}))
			}
			s.wg.Add(1)
			go s.acceptForward(conn, fl, p.Host, boundPort)
		case "cancel-tcpip-forward":
			// We don't bother matching to the specific listener — closing
			// all of them on conn-end is enough for tests.
			if r.WantReply {
				_ = r.Reply(true, nil)
			}
		default:
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}
}

// acceptForward listens on the locally-bound port and opens a forwarded-tcpip
// channel back to the client for each accept. This is what makes reverse
// proxies work.
func (s *Server) acceptForward(conn *ssh.ServerConn, fl net.Listener, addr string, port uint32) {
	defer s.wg.Done()
	defer fl.Close()
	for {
		c, err := fl.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			origHost, origPortStr, _ := net.SplitHostPort(c.RemoteAddr().String())
			origPortI, _ := strconv.Atoi(origPortStr)
			payload := ssh.Marshal(struct {
				Addr       string
				Port       uint32
				OriginAddr string
				OriginPort uint32
			}{addr, port, origHost, uint32(origPortI)})
			ch, reqs, err := conn.OpenChannel("forwarded-tcpip", payload)
			if err != nil {
				return
			}
			go ssh.DiscardRequests(reqs)
			defer ch.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(ch, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, ch); done <- struct{}{} }()
			<-done
		}(c)
	}
}

func (s *Server) handleSession(ch ssh.Channel, in <-chan *ssh.Request) {
	defer ch.Close()
	for r := range in {
		switch r.Type {
		case "exec":
			if !s.opts.AllowExec {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			var p struct{ Command string }
			_ = ssh.Unmarshal(r.Payload, &p)
			_ = r.Reply(true, nil)
			code := 0
			if s.opts.ExecHandler != nil {
				code = s.opts.ExecHandler(p.Command, ch)
			} else {
				_, _ = io.WriteString(ch, "EXEC: "+p.Command+"\n")
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			return
		case "subsystem":
			if !s.opts.AllowSFTP {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			var p struct{ Subsystem string }
			_ = ssh.Unmarshal(r.Payload, &p)
			if p.Subsystem != "sftp" {
				_ = r.Reply(false, nil)
				continue
			}
			_ = r.Reply(true, nil)
			var srvOpts []sftp.ServerOption
			if s.opts.SFTPRoot != "" {
				srvOpts = append(srvOpts, sftp.WithServerWorkingDirectory(s.opts.SFTPRoot))
			}
			srv, err := sftp.NewServer(ch, srvOpts...)
			if err != nil {
				return
			}
			_ = srv.Serve()
			return
		case "pty-req":
			if r.WantReply {
				_ = r.Reply(s.opts.AllowPTY, nil)
			}
		case "shell":
			if !s.opts.AllowPTY {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
				continue
			}
			_ = r.Reply(true, nil)
			_, _ = io.WriteString(ch, "shell\n")
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		case "window-change":
			if r.WantReply {
				_ = r.Reply(true, nil)
			}
		default:
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}
}

func (s *Server) handleDirectTCPIP(nc ssh.NewChannel) {
	var p struct {
		Host       string
		Port       uint32
		Originator string
		OrigPort   uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	dest := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
	out, err := net.DialTimeout("tcp", dest, 5*time.Second)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		out.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { defer ch.Close(); defer out.Close(); _, _ = io.Copy(ch, out) }()
	go func() { defer ch.Close(); defer out.Close(); _, _ = io.Copy(out, ch) }()
}
