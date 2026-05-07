package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
)

// classifyDialError wraps a TCP dial error with a human-readable hint.
// We try to recognise a small set of common failure modes; everything else
// falls through with a generic message.
func classifyDialError(addr string, err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("dial %s timed out — firewall blocking, wrong port, or VPN down? (%w)", addr, err)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("dial %s refused — sshd not running, or wrong port? (%w)", addr, err)
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return fmt.Errorf("dial %s unreachable — VPN down, or typo in addr? (%w)", addr, err)
	}

	// DNS lookup failure has no portable sentinel; check by type.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("dial %s: can't resolve hostname %q (%w)", addr, dnsErr.Name, err)
	}

	return fmt.Errorf("dial %s: %w", addr, err)
}

// classifyHandshakeError wraps an SSH handshake error with a hint.
func classifyHandshakeError(addr string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()

	// Our own host-key errors come back through here wrapped in the
	// handshake error. Surface them as-is without the misleading
	// "wrong port (non-SSH service)" hint that the bare-EOF check below
	// would produce.
	if errors.Is(err, ErrHostKeyNoTTY) || errors.Is(err, ErrHostKeyRejected) {
		return fmt.Errorf("ssh %s: %w", addr, err)
	}
	if strings.Contains(msg, "HOST KEY MISMATCH") {
		return err
	}

	// gssapi-with-mic is the canonical Kerberos auth method on most
	// corporate jump hosts (ByteDance, Google, etc). x/crypto/ssh has no
	// GSSAPI support, so if it's the only method the server offers we
	// have to bail with an actionable message.
	if strings.Contains(msg, "gssapi-with-mic") &&
		(strings.Contains(msg, "no supported methods remain") || strings.Contains(msg, "unable to authenticate")) {
		return fmt.Errorf("ssh %s: server requires GSSAPI/Kerberos auth which tn does not support — use the system ssh to open a tunnel and point tn at the local port (see README \"Behind a Kerberos jump host\") (%w)", addr, err)
	}

	switch {
	case errors.Is(err, io.EOF):
		return fmt.Errorf("ssh %s: server closed the connection during handshake — wrong port (talking to a non-SSH service)? (%w)", addr, err)
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods remain"):
		return fmt.Errorf("ssh %s: auth failed — wrong password/key, or key not in authorized_keys (try `tn upload-key` after a password connect) (%w)", addr, err)
	case strings.Contains(msg, "ssh: handshake failed"), strings.Contains(msg, "protocol version"):
		return fmt.Errorf("ssh %s: handshake failed — server might not be sshd (%w)", addr, err)
	}
	return fmt.Errorf("ssh %s: %w", addr, err)
}
