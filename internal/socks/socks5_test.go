package socks

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestSocks5DomainConnect drives a real SOCKS5 client handshake against the
// server, then reads/writes through it via an in-memory upstream.
func TestSocks5DomainConnect(t *testing.T) {
	// Upstream "echo" listener; the SOCKS server's dial points at it.
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		conn, err := up.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	// SOCKS server.
	sl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()

	// Dialer that always returns to the upstream (ignoring requested host).
	dialer := func(network, address string) (net.Conn, error) {
		return net.Dial("tcp", up.Addr().String())
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = Serve(sl, dialer) }()

	c, err := net.Dial("tcp", sl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	// Greeting.
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("greeting reply = %v", resp)
	}

	// CONNECT example.com:80 (domain atyp).
	host := "example.com"
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	port := []byte{0, 80}
	req = append(req, port...)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[1] != 0x00 {
		t.Fatalf("CONNECT reply rep = %d", hdr[1])
	}
	// Drain bound addr+port (we always reply ATYP=1 → 4+2 bytes).
	if _, err := io.ReadFull(c, make([]byte, 6)); err != nil {
		t.Fatal(err)
	}

	// Now data is plumbed through.
	payload := []byte("hello tune")
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}

	c.Close()
	sl.Close()
	wg.Wait()
}

func TestSocks5UnsupportedCommand(t *testing.T) {
	sl, _ := net.Listen("tcp", "127.0.0.1:0")
	defer sl.Close()
	go Serve(sl, nil)

	c, err := net.Dial("tcp", sl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))

	c.Write([]byte{0x05, 0x01, 0x00})
	io.ReadFull(c, make([]byte, 2))
	// BIND command (0x02) is not supported.
	c.Write([]byte{0x05, 0x02, 0x00, 0x01, 127, 0, 0, 1, 0, 80})

	resp := make([]byte, 10)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[1] != 0x07 { // command not supported
		t.Errorf("expected rep=0x07 (command not supported), got 0x%02x", resp[1])
	}
}

// Compile-time check that binary import is used in expectations.
var _ = binary.BigEndian
