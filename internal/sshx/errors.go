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

	switch {
	case errors.Is(err, io.EOF):
		return fmt.Errorf("ssh %s: server closed the connection during handshake — wrong port (talking to a non-SSH service)? (%w)", addr, err)
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods remain"):
		return fmt.Errorf("ssh %s: auth failed — wrong password/key, or key not in authorized_keys (try `tn upload-key` after a password connect) (%w)", addr, err)
	case strings.Contains(msg, "ssh: handshake failed"), strings.Contains(msg, "protocol version"):
		return fmt.Errorf("ssh %s: handshake failed — server might not be sshd (%w)", addr, err)
	case strings.Contains(msg, "HOST KEY MISMATCH"):
		// Already friendly.
		return err
	}
	return fmt.Errorf("ssh %s: %w", addr, err)
}
