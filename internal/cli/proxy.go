package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/socks"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/spf13/cobra"
)

var (
	proxyPort      int
	proxyBind      string
	proxyWrite     bool
	proxyReconnect bool
)

// proxyEnvPath is the absolute path of proxy.env on the remote, written when
// --write-env is set. Kept here so the cleanup path uses the same string.
const proxyEnvPath = ".tn/proxy.env"

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Run a reverse SOCKS5 proxy on the remote, served by the local network",
	Long: `Listens on the remote host (default 127.0.0.1:1080) and serves SOCKS5
locally — every TCP connection from the remote through this proxy uses your
local machine's network. Useful when the remote can't reach the public
internet but you can.

In another shell on the remote, set:

    export ALL_PROXY=socks5h://127.0.0.1:1080
    export HTTP_PROXY=socks5h://127.0.0.1:1080
    export HTTPS_PROXY=socks5h://127.0.0.1:1080

Then pip/npm/curl/git/apt-get https traffic will egress from your local box.

With --write-env, an env-file is dropped at $HOME/.tn/proxy.env on the remote,
which "tn exec --proxy" sources automatically. The file is removed when this
command exits cleanly so a stale env file doesn't trick later "tn exec --proxy"
runs into pointing at a dead listener.

With --reconnect (default), the proxy auto-reopens the SSH connection on
*transient* failures (network drops, SSH session killed). Configuration
errors — e.g. the remote port already being in use — exit immediately
without retrying.`,
	Example: `  tn proxy                          # remote listens on 127.0.0.1:1080
  tn proxy -p 0                     # auto-pick a free port (good for shared hosts)
  tn proxy -p 8443 --write-env=false`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		host, err := cfg.Resolve(flagHost)
		if err != nil {
			return err
		}
		policy := currentPolicy()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintln(os.Stderr, "tn proxy: stopping")
			cancel()
		}()

		backoff := time.Second
		const maxBackoff = 30 * time.Second
		for {
			err := runProxyOnce(ctx, host, policy)
			if ctx.Err() != nil {
				return nil
			}
			if !proxyReconnect || isPermanentProxyError(err) {
				return err
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "tn proxy: %v — reconnecting in %s\n", err, backoff)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	},
}

// isPermanentProxyError tells the reconnect loop to give up. Anything that
// won't fix itself by waiting (port-in-use, auth failure) goes here.
func isPermanentProxyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "tcpip-forward request denied") ||
		strings.Contains(msg, "auth failed") ||
		strings.Contains(msg, "GSSAPI/Kerberos") ||
		strings.Contains(msg, "HOST KEY MISMATCH")
}

// runProxyOnce dials the host, opens a remote listener, serves SOCKS5 over
// it, and returns when the listener errors or ctx is cancelled. Caller is
// responsible for the reconnect loop.
func runProxyOnce(ctx context.Context, host *config.Host, policy sshx.HostKeyPolicy) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, flagTimeout)
	c, err := sshx.Dial(dialCtx, host, policy)
	dialCancel()
	if err != nil {
		return err
	}
	defer c.Close()

	addr := net.JoinHostPort(proxyBind, strconv.Itoa(proxyPort))
	l, err := c.SSH().Listen("tcp", addr)
	if err != nil {
		return wrapListenErr(addr, err)
	}
	defer l.Close()

	_, listenPort, _ := net.SplitHostPort(l.Addr().String())

	if proxyWrite {
		fc, ferr := newSFTP(c)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "tn proxy: skipping --write-env: %v\n", ferr)
		} else {
			envBody := fmt.Sprintf(`# written by tn proxy — removed automatically when tn proxy exits
# tn-proxy-port=%s
export ALL_PROXY=socks5h://127.0.0.1:%s
export HTTP_PROXY=socks5h://127.0.0.1:%s
export HTTPS_PROXY=socks5h://127.0.0.1:%s
export NO_PROXY=localhost,127.0.0.1
`, listenPort, listenPort, listenPort, listenPort)
			if err := fc.MkdirAll(".tn"); err != nil {
				fmt.Fprintf(os.Stderr, "tn proxy: mkdir ~/.tn on remote: %v\n", err)
			}
			if err := writeFile(fc, proxyEnvPath, strings.NewReader(envBody)); err != nil {
				fmt.Fprintf(os.Stderr, "tn proxy: write proxy.env: %v\n", err)
			}
			// Best-effort cleanup so the next agent invocation doesn't read
			// stale data. We use a fresh sftp client because the original
			// might already be closed by the deferred c.Close().
			defer func() {
				cleanup, cerr := newSFTP(c)
				if cerr != nil {
					return
				}
				_ = cleanup.Remove(proxyEnvPath)
				cleanup.Close()
			}()
			fc.Close()
		}
	}

	fmt.Fprintf(os.Stderr, "tn proxy: listening on remote %s — Ctrl-C to stop\n", l.Addr())
	fmt.Fprintf(os.Stderr, "  remote setup:\n    export ALL_PROXY=socks5h://127.0.0.1:%s\n", listenPort)

	// Close the listener when ctx is cancelled so socks.Serve returns.
	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
		case <-stopCh:
		}
	}()

	return socks.Serve(l, nil)
}

// wrapListenErr translates the cryptic "ssh: tcpip-forward request denied by
// peer" into something agent-readable when we can guess why.
func wrapListenErr(addr string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "tcpip-forward request denied") {
		_, port, _ := net.SplitHostPort(addr)
		if port == "0" {
			// Auto-pick was rejected — server-wide forwarding is off.
			return fmt.Errorf("remote rejected tcpip-forward — sshd may have AllowTcpForwarding=no or DisableForwarding=yes (%w)", err)
		}
		return fmt.Errorf("remote rejected tcpip-forward on %s — port %s likely already in use; try `-p 0` to auto-pick a free port (%w)", addr, port, err)
	}
	return fmt.Errorf("remote listen %s: %w", addr, err)
}

// errProxyListenDenied is exposed for tests that want to assert the
// classifier triggers.
var errProxyListenDenied = errors.New("tcpip-forward request denied")

func init() {
	proxyCmd.Flags().IntVarP(&proxyPort, "port", "p", 1080, "remote port to listen on (0 = pick free)")
	proxyCmd.Flags().StringVar(&proxyBind, "bind", "127.0.0.1", "remote bind address (use 0.0.0.0 to expose to other remote users — discouraged)")
	proxyCmd.Flags().BoolVar(&proxyWrite, "write-env", true, "drop ~/.tn/proxy.env on remote so 'tn exec --proxy' picks it up")
	proxyCmd.Flags().BoolVar(&proxyReconnect, "reconnect", true, "auto-reopen SSH on transient failures, with exponential backoff")
}
