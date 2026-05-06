// Package sshx is a thin wrapper around golang.org/x/crypto/ssh that knows
// how to dial a target through a jumpbox using a single TCP connection per
// hop and to keep the resulting client healthy.
//
// Architecture:
//
//	local --TCP--> jump (ssh) --SSH-channel--> target (ssh)
//
// The jump connection is a regular ssh.Client; the inner connection is an
// SSH handshake performed over a "direct-tcpip" channel that the jump opens
// for us. The resulting target client supports all SSH features (sessions,
// SFTP, remote port forwarding) without OS-level processes or socket files.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cklxx/tune/internal/config"
	"golang.org/x/crypto/ssh"
)

// Client wraps an ssh.Client to the target plus the optional jump client that
// underlies it. Close() tears down both. It is safe to call Exec/Dial/etc.
// from multiple goroutines.
type Client struct {
	cfg    *config.Host
	target *ssh.Client
	jump   *ssh.Client
	mu     sync.Mutex
	closed atomic.Bool
}

// Dial opens a connection per cfg. ctx applies to TCP/SSH handshakes only;
// once Dial returns, ctx is no longer consulted.
func Dial(ctx context.Context, cfg *config.Host, policy HostKeyPolicy) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("nil host config")
	}
	hkcb, err := HostKeyCallback(cfg.KnownHosts, policy)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

	c := &Client{cfg: cfg}

	var underlying net.Conn
	var sshAddr string

	if cfg.Jump != nil {
		jumpAddr := config.EnsureAddrPort(cfg.Jump.Addr)
		methods, err := AuthMethods(*cfg.Jump, fmt.Sprintf("%s@%s", cfg.Jump.User, jumpAddr))
		if err != nil {
			return nil, fmt.Errorf("jump auth: %w", err)
		}
		jumpCfg := &ssh.ClientConfig{
			User:            cfg.Jump.User,
			Auth:            methods,
			HostKeyCallback: hkcb,
			Timeout:         15 * time.Second,
		}
		conn, err := dialer.DialContext(ctx, "tcp", jumpAddr)
		if err != nil {
			return nil, fmt.Errorf("dial jump %s: %w", jumpAddr, err)
		}
		jc, ch, reqs, err := ssh.NewClientConn(conn, jumpAddr, jumpCfg)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("ssh handshake jump: %w", err)
		}
		c.jump = ssh.NewClient(jc, ch, reqs)
		// Open a direct-tcpip channel from the jump to the target.
		targetAddr := config.EnsureAddrPort(cfg.Target.Addr)
		inner, err := c.jump.DialContext(ctx, "tcp", targetAddr)
		if err != nil {
			c.jump.Close()
			return nil, fmt.Errorf("dial target through jump: %w", err)
		}
		underlying = inner
		sshAddr = targetAddr
	} else {
		targetAddr := config.EnsureAddrPort(cfg.Target.Addr)
		conn, err := dialer.DialContext(ctx, "tcp", targetAddr)
		if err != nil {
			return nil, fmt.Errorf("dial target %s: %w", targetAddr, err)
		}
		underlying = conn
		sshAddr = targetAddr
	}

	methods, err := AuthMethods(cfg.Target, fmt.Sprintf("%s@%s", cfg.Target.User, sshAddr))
	if err != nil {
		underlying.Close()
		if c.jump != nil {
			c.jump.Close()
		}
		return nil, fmt.Errorf("target auth: %w", err)
	}
	tcfg := &ssh.ClientConfig{
		User:            cfg.Target.User,
		Auth:            methods,
		HostKeyCallback: hkcb,
		Timeout:         15 * time.Second,
	}
	tc, ch, reqs, err := ssh.NewClientConn(underlying, sshAddr, tcfg)
	if err != nil {
		underlying.Close()
		if c.jump != nil {
			c.jump.Close()
		}
		return nil, fmt.Errorf("ssh handshake target: %w", err)
	}
	c.target = ssh.NewClient(tc, ch, reqs)

	// Periodic keepalive: server-aliveness + NAT.
	go c.keepalive()

	return c, nil
}

// SSH returns the underlying ssh.Client to the target. Callers should not
// close it directly — use Client.Close.
func (c *Client) SSH() *ssh.Client { return c.target }

// Close tears down both the target and (if any) the jump connections.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	if c.target != nil {
		if err := c.target.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.jump != nil {
		if err := c.jump.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Ping issues a no-op global request and measures round-trip time. Useful for
// health checks.
func (c *Client) Ping() (time.Duration, error) {
	if c.target == nil {
		return 0, errors.New("not connected")
	}
	start := time.Now()
	_, _, err := c.target.SendRequest("keepalive@tn", true, nil)
	if err != nil {
		// OpenSSH returns "request failed" for unknown global requests, which
		// still proves the channel is up.
		if err.Error() == "request failed" {
			return time.Since(start), nil
		}
		return 0, err
	}
	return time.Since(start), nil
}

func (c *Client) keepalive() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for !c.closed.Load() {
		select {
		case <-ticker.C:
			if c.target == nil {
				return
			}
			_, _, _ = c.target.SendRequest("keepalive@tn", true, nil)
		}
	}
}
