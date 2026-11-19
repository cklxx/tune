package cli

import (
	"errors"
	"net"
	"os"
	"os/exec"

	"github.com/cklxx/tune/internal/config"
)

// Some pooled hosts cannot be reached by tn's Go SSH client at all: they accept
// GSSAPI only, and golang.org/x/crypto/ssh has no GSSAPI support. Hosts marked
// transport: ssh, and hosts tagged bastion-only (a jump server that permits an
// interactive session and nothing else), are therefore executed through
// /usr/bin/ssh, which does speak Kerberos. Config stays the single source of
// truth; only the transport differs.

// resolveHost loads the --host entry without dialing.
func resolveHost() (*config.Host, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return cfg.Resolve(flagHost)
}

// usesSystemSSH reports whether the host must go through /usr/bin/ssh.
func usesSystemSSH(h *config.Host) bool {
	return h.Transport == "ssh" || hasTag(*h, "bastion-only")
}

// runSystemSSH runs args (or an interactive shell when args is empty) through
// the system ssh binary and propagates the remote exit status.
func runSystemSSH(h *config.Host, args []string, tty bool) error {
	cmdline := ""
	if len(args) > 0 {
		cmdline = joinShell(args)
	}

	var c *exec.Cmd
	if hasTag(*h, "bastion-only") {
		ip := h.Target.Addr
		if host, _, err := net.SplitHostPort(config.EnsureAddrPort(ip)); err == nil {
			ip = host
		}
		if cmdline == "" {
			c = exec.Command(config.Home()+"/bastion.sh", ip)
		} else {
			c = exec.Command(config.Home()+"/bastion.sh", ip, cmdline)
		}
	} else {
		argv := sshArgs(*h)
		if tty {
			argv = append([]string{"-t"}, argv...)
		}
		if cmdline != "" {
			argv = append(argv, cmdline)
		}
		c = exec.Command("/usr/bin/ssh", argv...)
	}

	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	return err
}
