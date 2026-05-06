package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
)

var (
	flagHost      string
	flagJSON      bool
	flagInsecure  bool
	flagTimeout   time.Duration
)

func registerCommonFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVarP(&flagHost, "host", "H", "", "host alias from config (overrides $TN_HOST and defaultHost)")
	cmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable JSON output where supported")
	cmd.PersistentFlags().BoolVar(&flagInsecure, "insecure-host-key", false, "accept any host key (DANGEROUS — for ad-hoc/testing only)")
	cmd.PersistentFlags().DurationVar(&flagTimeout, "timeout", 30*time.Second, "dial timeout")
}

// connect resolves a host from config and dials it. The returned client must
// be closed by the caller.
func connect() (*sshx.Client, *config.Host, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	host, err := cfg.Resolve(flagHost)
	if err != nil {
		return nil, nil, err
	}
	policy := sshx.PolicyTOFU
	if flagInsecure {
		policy = sshx.PolicyInsecure
	}
	ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
	defer cancel()
	c, err := sshx.Dial(ctx, host, policy)
	if err != nil {
		return nil, host, err
	}
	return c, host, nil
}

func newSFTP(c *sshx.Client) (*sftp.Client, error) {
	return sftp.NewClient(c.SSH(),
		sftp.UseConcurrentReads(true),
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(64),
	)
}

// die prints err and exits 1, formatting nicely for the user.
func die(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "tn:", err)
	os.Exit(1)
}

