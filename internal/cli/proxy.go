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

	"github.com/cklxx/tune/internal/socks"
	"github.com/spf13/cobra"
)

var (
	proxyPort  int
	proxyBind  string
	proxyWrite bool
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
which "tn exec --proxy" sources automatically.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := connect()
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

		// Effective port (in case 0 was requested).
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

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Stop on signal.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() { <-sigCh; cancel(); l.Close() }()

		// Local dialer is plain — that's the whole point: outbound from local.
		errCh := make(chan error, 1)
		go func() { errCh <- socks.Serve(l, nil) }()

		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return nil
		}
	},
}

func init() {
	proxyCmd.Flags().IntVarP(&proxyPort, "port", "p", 1080, "remote port to listen on (0 = pick free)")
	proxyCmd.Flags().StringVar(&proxyBind, "bind", "127.0.0.1", "remote bind address (use 0.0.0.0 to expose to other remote users — discouraged)")
	proxyCmd.Flags().BoolVar(&proxyWrite, "write-env", true, "drop ~/.tn/proxy.env on remote so 'tn exec --proxy' picks it up")
}

