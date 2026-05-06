package sshx_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/socks"
	"github.com/cklxx/tune/internal/sshtest"
	"github.com/cklxx/tune/internal/sshx"
)

// TestReverseSocksThroughSSH wires up the real "remote uses local network"
// scenario end-to-end:
//
//  1. Boot an SSH test server with tcpip-forward enabled (acts as the
//     remote host).
//  2. Boot a plain TCP echo server playing the role of "the public internet"
//     reachable from the local box.
//  3. Dial the SSH server, then use ssh.Client.Listen to ask the SERVER to
//     listen — connections that arrive on that server-side listener come back
//     to us as channels.
//  4. Run our SOCKS5 server over those channels with a dialer that connects
//     to the echo server.
//  5. Connect a SOCKS5 client to the SERVER's listening port (simulating a
//     remote process). Send bytes through. Verify they round-trip via the
//     local echo server.
//
// This validates: tcpip-forward, forwarded-tcpip channel handling in
// crypto/ssh, our SOCKS5 implementation, and the end-to-end story for
// `tn proxy`.
func TestReverseSocksThroughSSH(t *testing.T) {
	// 1. Echo "public internet" on the local side.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	// 2. SSH server with tcpip-forward.
	kp := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{
		AllowedKey:   kp.PublicKey,
		AllowForward: true,
	})

	// 3. Dial via sshx.
	host := &config.Host{
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

	// Ask the remote to listen on a free port. Connections will be channelled
	// back to us as net.Conns yielded by l.Accept.
	l, err := c.SSH().Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("remote listen: %v", err)
	}
	defer l.Close()

	// 4. Serve SOCKS5 on that "remote" listener, dialing locally to echo.
	go func() {
		_ = socks.Serve(l, func(network, addr string) (net.Conn, error) {
			// Ignore the requested addr — always send to our echo for testing.
			return net.Dial("tcp", echo.Addr().String())
		})
	}()

	// 5. Connect a SOCKS client to the remote-listener port (which the test
	// server is actually listening on locally — that's the whole point: the
	// server's net.Listen is the "remote" side).
	clientConn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial socks listener: %v", err)
	}
	defer clientConn.Close()
	clientConn.SetDeadline(time.Now().Add(5 * time.Second))

	// SOCKS5 greeting (no auth).
	if _, err := clientConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("greeting: %v", resp)
	}

	// CONNECT example.com:80 (domain name; dialer ignores it anyway).
	host2 := "example.com"
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host2))}
	req = append(req, []byte(host2)...)
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, 80)
	req = append(req, port...)
	if _, err := clientConn.Write(req); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(clientConn, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[1] != 0x00 {
		t.Fatalf("CONNECT rep = %d", hdr[1])
	}
	if _, err := io.ReadFull(clientConn, make([]byte, 6)); err != nil {
		t.Fatal(err)
	}

	// Plumbed through. Bytes go: clientConn → SSH listener (Accept) →
	// our socks.Serve → dialer → echo → back through everything.
	payload := []byte("via tn proxy")
	if _, err := clientConn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(clientConn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, payload)
	}
}
