package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/cklxx/tune/internal/config"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init [name]",
	Short: "Interactively register a host in ~/.tn/config.yaml",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		r := bufio.NewReader(os.Stdin)

		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		if name == "" {
			name = ask(r, "host alias", "default")
		}

		host := cfg.Hosts[name]
		if host == nil {
			host = &config.Host{Name: name}
		}

		host.Target.Addr = ask(r, "target addr (host[:port])", host.Target.Addr)
		host.Target.User = ask(r, "target user", host.Target.User)
		host.Target.IdentityFile = ask(r, "target identity file (optional)", host.Target.IdentityFile)
		host.Target.PasswordCmd = ask(r, "target passwordCmd (optional, e.g. 'pass show ssh/host')", host.Target.PasswordCmd)

		if yes(r, "use a jump host?", host.Jump != nil) {
			if host.Jump == nil {
				host.Jump = &config.Hop{}
			}
			host.Jump.Addr = ask(r, "jump addr (host[:port])", host.Jump.Addr)
			host.Jump.User = ask(r, "jump user", host.Jump.User)
			host.Jump.IdentityFile = ask(r, "jump identity file (optional)", host.Jump.IdentityFile)
			host.Jump.PasswordCmd = ask(r, "jump passwordCmd (optional)", host.Jump.PasswordCmd)
		} else {
			host.Jump = nil
		}

		cfg.Hosts[name] = host
		if cfg.DefaultHost == "" || len(cfg.Hosts) == 1 {
			cfg.DefaultHost = name
		}

		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "saved host %q to %s\n", name, fmt.Sprintf("%s/config.yaml", config.Home()))
		fmt.Fprintln(cmd.OutOrStdout(), "try:  tn status")
		return nil
	},
}

func ask(r *bufio.Reader, prompt, def string) string {
	if def == "" {
		fmt.Fprintf(os.Stderr, "%s: ", prompt)
	} else {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", prompt, def)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return def
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def
	}
	return line
}

func yes(r *bufio.Reader, prompt string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(os.Stderr, "%s [%s]: ", prompt, hint)
	line, err := r.ReadString('\n')
	if err != nil {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}
