package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/cklxx/tune/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// PasswordPrompt is overrideable for testing.
var PasswordPrompt = func(prompt string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("password required but stdin is not a terminal")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

// AuthMethods builds the auth chain for one hop, in priority order:
//
//  1. Identity file (if configured and readable)
//  2. SSH agent (if SSH_AUTH_SOCK is set)
//  3. passwordCmd output (lazy: only invoked if earlier methods are skipped or
//     fail server-side)
//  4. Interactive prompt (terminal only)
//
// Returns at least one method or an error. Order matters: ssh-go tries them
// in sequence and stops at the first that the server accepts.
func AuthMethods(hop config.Hop, label string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if path := config.ExpandPath(hop.IdentityFile); path != "" {
		signer, err := loadIdentity(path)
		if err != nil {
			return nil, fmt.Errorf("identity %s: %w", path, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}

	// password (cmd or prompt) — wrap as a callback so it's only invoked when
	// the server actually challenges for a password.
	pw := passwordSource(hop, label)
	methods = append(methods, ssh.PasswordCallback(pw))
	methods = append(methods, ssh.RetryableAuthMethod(ssh.PasswordCallback(pw), 2))

	if len(methods) == 0 {
		return nil, errors.New("no auth methods available")
	}
	return methods, nil
}

func passwordSource(hop config.Hop, label string) func() (string, error) {
	var cached string
	return func() (string, error) {
		if cached != "" {
			return cached, nil
		}
		if hop.PasswordCmd != "" {
			out, err := exec.Command("/bin/sh", "-c", hop.PasswordCmd).Output()
			if err != nil {
				return "", fmt.Errorf("passwordCmd: %w", err)
			}
			cached = strings.TrimRight(string(out), "\r\n")
			return cached, nil
		}
		pw, err := PasswordPrompt(fmt.Sprintf("password for %s: ", label))
		if err != nil {
			return "", err
		}
		cached = pw
		return cached, nil
	}
}

func loadIdentity(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer, nil
	}
	// Encrypted key — prompt for passphrase.
	var pe *ssh.PassphraseMissingError
	if errors.As(err, &pe) {
		pp, perr := PasswordPrompt(fmt.Sprintf("passphrase for %s: ", path))
		if perr != nil {
			return nil, perr
		}
		return ssh.ParsePrivateKeyWithPassphrase(data, []byte(pp))
	}
	return nil, err
}
