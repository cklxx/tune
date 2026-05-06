package cli

import (
	"context"
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
which "tn exec --proxy" sources automatically.

With --reconnect (default), the proxy auto-reopens the SSH connection on
transient failures with exponential backoff, capped at 30s. This makes
"tn proxy" resilient to flaky networks where SSH might briefly drop.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		host, err := cfg.Resolve(flagHost)
		if err != nil {
			return err
		}
		policy := sshx.PolicyTOFU
		if flagInsecure {
			policy = sshx.PolicyInsecure
		}

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
			if !proxyReconnect {
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
		return fmt.Errorf("remote listen %s: %w", addr, err)
	}
	defer l.Close()

	_, listenPort, _ := net.SplitHostPort(l.Addr().String())

	if proxyWrite {
		fc, err := newSFTP(c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tn proxy: skipping --write-env: %v\n", err)
		} else {
			envBody := fmt.Sprintf("export ALL_PROXY=socks5h://127.0.0.1:%s\nexport HTTP_PROXY=socks5h://127.0.0.1:%s\nexport HTTPS_PROXY=socks5h://127.0.0.1:%s\nexport NO_PROXY=localhost,127.0.0.1\n",
				listenPort, listenPort, listenPort)
			if err := fc.MkdirAll(".tn"); err != nil {
				fmt.Fprintf(os.Stderr, "tn proxy: mkdir ~/.tn on remote: %v\n", err)
			}
			if err := writeFile(fc, ".tn/proxy.env", strings.NewReader(envBody)); err != nil {
				fmt.Fprintf(os.Stderr, "tn proxy: write proxy.env: %v\n", err)
			}
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

func init() {
	proxyCmd.Flags().IntVarP(&proxyPort, "port", "p", 1080, "remote port to listen on (0 = pick free)")
	proxyCmd.Flags().StringVar(&proxyBind, "bind", "127.0.0.1", "remote bind address (use 0.0.0.0 to expose to other remote users — discouraged)")
	proxyCmd.Flags().BoolVar(&proxyWrite, "write-env", true, "drop ~/.tn/proxy.env on remote so 'tn exec --proxy' picks it up")
	proxyCmd.Flags().BoolVar(&proxyReconnect, "reconnect", true, "auto-reopen SSH on transient failures, with exponential backoff")
}
