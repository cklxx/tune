// Package cli wires up the tn cobra commands.
package cli

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "tn",
	Short: "Fast SSH tooling for humans and coding agents",
	Long: `tn is a CLI for working against a remote SSH host (optionally through a
jumpbox) with a focus on:

  * fast, scriptable file and command operations,
  * a reverse SOCKS5 proxy so the remote can use the local network for
    package installs (pip, npm, apt, etc.),
  * trust-on-first-use host-key pinning and password-or-key auth.

Configure hosts with "tn init", then use "tn exec", "tn push", "tn pull",
"tn read", "tn write", "tn ls", "tn proxy", "tn shell", and "tn status".`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// SetVersion sets the version string shown by `tn --version`. main() is
// expected to call this with a value injected at link time via:
//
//	go build -ldflags "-X main.version=v0.1.0"
//
// goreleaser does this automatically.
func SetVersion(v string) {
	if v == "" {
		v = "dev"
	}
	rootCmd.Version = v
}

// Execute runs the root cobra command. It returns the error returned by the
// command (or nil); main() is expected to exit non-zero on a non-nil error.
func Execute() error {
	registerCommonFlags(rootCmd)
	rootCmd.AddCommand(
		initCmd,
		execCmd,
		pushCmd,
		pullCmd,
		readCmd,
		writeCmd,
		lsCmd,
		proxyCmd,
		shellCmd,
		statusCmd,
		benchCmd,
		uploadKeyCmd,
		doctorCmd,
		mirrorCmd,
	)
	return rootCmd.Execute()
}
