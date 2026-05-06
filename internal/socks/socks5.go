// Package socks implements a minimal SOCKS5 server (RFC 1928) supporting the
// CONNECT command with no authentication. It is intended to be driven by a
// remote SSH listener so that processes on the remote host can route TCP via
// the local machine's network — e.g.:
//
//	# remote shell:
//	ALL_PROXY=socks5h://127.0.0.1:1080 pip install foo
//
// Domain names are resolved on the local side, so private DNS visible only to
// the local network works as expected (this is what socks5h:// means).
package socks

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Dialer is the function used to open the outbound side of a CONNECT.
// Replacing it lets callers swap in custom resolvers or per-destination
// policies. The default is (&net.Dialer{Timeout:30s}).DialContext-equivalent.
type Dialer func(network, address string) (net.Conn, error)

// DefaultDialer dials with a 30s timeout.
var DefaultDialer Dialer = func(network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	return d.Dial(network, address)
}

// Serve accepts SOCKS5 clients on listener until it errors, dispatching each
// to dial. Closing listener returns nil.
func Serve(listener net.Listener, dial Dialer) error {
	if dial == nil {
		dial = DefaultDialer
	}
	for {
		c, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func(conn net.Conn) {
			defer conn.Close()
			if err := handle(conn, dial); err != nil && !errors.Is(err, io.EOF) {
				// best-effort log via stderr — caller may redirect
			}
		}(c)
	}
}

func handle(c net.Conn, dial Dialer) error {
	c.SetDeadline(time.Now().Add(15 * time.Second))

	// Greeting: VER NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x05 {
		return fmt.Errorf("unsupported socks version %d", hdr[0])
	}
	if _, err := io.ReadFull(c, make([]byte, hdr[1])); err != nil {
		return err
	}
	// Reply: VER METHOD (0x00 = no auth)
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return err
	}

	// Request: VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return err
	}
	if req[0] != 0x05 {
		return fmt.Errorf("bad request version")
	}
	cmd := req[1]
	atyp := req[3]

	var host string
	switch atyp {
	case 0x01: // IPv4
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		host = net.IP(buf).String()
	case 0x03: // domain
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return err
		}
		buf := make([]byte, l[0])
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		host = string(buf)
	case 0x04: // IPv6
		buf := make([]byte, 16)
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		host = net.IP(buf).String()
	default:
		writeReply(c, 0x08, nil) // address type not supported
		return fmt.Errorf("unsupported atyp %d", atyp)
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(c, portBuf); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(portBuf)

	if cmd != 0x01 {
		writeReply(c, 0x07, nil) // command not supported
		return fmt.Errorf("unsupported cmd %d", cmd)
	}

	// Dial outbound (on the local side).
	c.SetDeadline(time.Time{})
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	out, err := dial("tcp", target)
	if err != nil {
		writeReply(c, 0x05, nil) // connection refused
		return err
	}
	defer out.Close()

	// Reply success with bound addr (we don't know it; zero is fine for clients).
	if err := writeReply(c, 0x00, out.LocalAddr()); err != nil {
		return err
	}

	pipe(c, out)
	return nil
}

func writeReply(c net.Conn, code byte, addr net.Addr) error {
	resp := []byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if a, ok := addr.(*net.TCPAddr); ok && a.IP.To4() != nil {
		copy(resp[4:8], a.IP.To4())
		binary.BigEndian.PutUint16(resp[8:10], uint16(a.Port))
	}
	_, err := c.Write(resp)
	return err
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); _ = setReadDeadline(a) }()
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); _ = setReadDeadline(b) }()
	wg.Wait()
}

func setReadDeadline(c net.Conn) error {
	// Nudge the other side's Copy to return.
	return c.SetReadDeadline(time.Now())
}
