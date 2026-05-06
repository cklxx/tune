// Command tn is the tune CLI: a fast SSH front-end with jumpbox support, a
// reverse SOCKS5 proxy, and SFTP-backed file ops, designed for both humans
// and coding agents.
package main

import (
	"fmt"
	"os"

	"github.com/cklxx/tune/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "tn:", err)
		os.Exit(1)
	}
}
