package sshx

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cklxx/tune/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// HostKeyPolicy controls what happens on an unknown host key.
type HostKeyPolicy int

const (
	// PolicyTOFU prompts the user once on first sight, then pins. With no
	// TTY available, returns ErrHostKeyNoTTY rather than blocking forever
	// or accepting silently.
	PolicyTOFU HostKeyPolicy = iota
	// PolicyStrict refuses any unknown host.
	PolicyStrict
	// PolicyInsecure accepts any host key. Use only for tests / ad-hoc.
	PolicyInsecure
	// PolicyAcceptNew pins on first sight without prompting. Mismatches on
	// already-known hosts are still fatal. Mirrors OpenSSH's
	// StrictHostKeyChecking=accept-new.
	PolicyAcceptNew
)

// ErrHostKeyNoTTY is returned by the host-key callback under PolicyTOFU when
// the host is unknown and there is no terminal to prompt on. Distinguished
// from a network EOF so the handshake error classifier can produce an
// actionable message instead of "wrong port".
var ErrHostKeyNoTTY = errors.New("unknown host key and stdin is not a terminal")

// ErrHostKeyRejected is returned when the user (or the strict policy)
// refuses to trust an unknown host.
var ErrHostKeyRejected = errors.New("host key rejected")

// HostKeyCallback returns a callback that consults the given known_hosts file
// (creating it if missing) and applies the policy on unknown keys. Concurrent
// calls are serialized to avoid duplicate appends.
func HostKeyCallback(path string, policy HostKeyPolicy) (ssh.HostKeyCallback, error) {
	if policy == PolicyInsecure {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	path = config.ExpandPath(path)
	if path == "" {
		path = filepath.Join(config.Home(), "known_hosts")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return nil, err
		}
	}

	var mu sync.Mutex
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		defer mu.Unlock()
		// Re-parse on each call so entries appended during this run are seen.
		check, err := knownhosts.New(path)
		if err != nil {
			return err
		}
		err = check(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) > 0 {
				// Mismatch: fail loudly. User can edit known_hosts to recover.
				return fmt.Errorf("HOST KEY MISMATCH for %s: server sent %s %s; expected one of %d pinned keys",
					hostname, key.Type(), ssh.FingerprintSHA256(key), len(keyErr.Want))
			}
			// Unknown — apply policy.
			switch policy {
			case PolicyStrict:
				return fmt.Errorf("%w: unknown host %s (%s %s) under strict policy",
					ErrHostKeyRejected, hostname, key.Type(), ssh.FingerprintSHA256(key))
			case PolicyAcceptNew:
				return appendKnownHost(path, hostname, key)
			case PolicyTOFU:
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("%w for %s (%s %s) — re-run with --accept-new to pin without prompting, --insecure-host-key to skip verification, or pre-populate %s",
						ErrHostKeyNoTTY, hostname, key.Type(), ssh.FingerprintSHA256(key), path)
				}
				ok, perr := promptTrust(hostname, key)
				if perr != nil {
					return perr
				}
				if !ok {
					return fmt.Errorf("%w for %s by user", ErrHostKeyRejected, hostname)
				}
				return appendKnownHost(path, hostname, key)
			}
		}
		return err
	}, nil
}

func promptTrust(hostname string, key ssh.PublicKey) (bool, error) {
	fmt.Fprintf(os.Stderr, "the authenticity of host %q can't be established.\n", hostname)
	fmt.Fprintf(os.Stderr, "%s key fingerprint is %s\n", key.Type(), ssh.FingerprintSHA256(key))
	fmt.Fprint(os.Stderr, "are you sure you want to continue connecting (yes/no)? ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{hostname}, key)
	_, err = fmt.Fprintln(f, line)
	return err
}
