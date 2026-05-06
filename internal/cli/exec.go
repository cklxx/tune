package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cklxx/tune/internal/sshx"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

var (
	execProxy bool
	execEnv   []string
	execCwd   string
)

var execCmd = &cobra.Command{
	Use:   "exec -- <cmd> [args...]",
	Short: "Run a command on the remote host and stream stdout/stderr",
	Long: `Runs a command on the remote, propagating exit codes and streaming
stdio. Arguments after "--" are quoted into a single shell command.

Examples:
  tn exec -- uname -a
  tn exec -- "ls -la /var/log | head"
  tn exec --cwd /srv/app -- go build ./...

With --proxy, sets HTTP_PROXY/HTTPS_PROXY/ALL_PROXY for the command — useful
to make package managers go through the local network. Requires "tn proxy"
to be running in another shell.`,
	Args:               cobra.MinimumNArgs(1),
	DisableFlagParsing: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()
		return runExec(c, args, execEnv, execCwd, execProxy)
	},
}

func init() {
	execCmd.Flags().BoolVar(&execProxy, "proxy", false, "inject HTTP(S)_PROXY/ALL_PROXY env from the running 'tn proxy'")
	execCmd.Flags().StringSliceVarP(&execEnv, "env", "e", nil, "extra environment, repeatable: -e KEY=VALUE")
	execCmd.Flags().StringVar(&execCwd, "cwd", "", "working directory on the remote")
}

// runExec executes args as a single shell command on the remote, piping
// local stdio. Returns the same exit code the remote process exited with.
func runExec(c *sshx.Client, args, env []string, cwd string, proxy bool) error {
	sess, err := c.SSH().NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	// Compose the command.
	cmdline := joinShell(args)
	prefix := ""
	if cwd != "" {
		prefix += fmt.Sprintf("cd %s && ", shQuote(cwd))
	}
	for _, kv := range env {
		prefix += "export " + envEscape(kv) + "; "
	}
	if proxy {
		// Best-effort: read $TN_PROXY_PORT from the remote env (set by `tn
		// proxy`'s status file). Falls back to 1080.
		prefix += "if [ -f \"$HOME/.tn/proxy.env\" ]; then . \"$HOME/.tn/proxy.env\"; fi; "
	}

	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	sess.Stdin = os.Stdin

	// Forward Ctrl-C: when our local SIGINT fires, send a SIGINT signal over
	// the SSH session so the remote process is interrupted, not just our pipe.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for s := range sigCh {
			switch s {
			case syscall.SIGINT:
				_ = sess.Signal(ssh.SIGINT)
			case syscall.SIGTERM:
				_ = sess.Signal(ssh.SIGTERM)
			}
		}
	}()

	err = sess.Run(prefix + cmdline)
	if err == nil {
		return nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitStatus())
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// joinShell produces a single shell command string from argv. If a single arg
// is given it's passed through (so "tn exec -- 'ls | head'" works); otherwise
// each arg is shell-quoted and joined with spaces.
func joinShell(argv []string) string {
	if len(argv) == 1 {
		return argv[0]
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shQuote(a)
	}
	return strings.Join(parts, " ")
}

// shQuote wraps s in single quotes, escaping any contained single quote.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`*?|&;<>(){}[]#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// envEscape splits "K=V" and quotes V.
func envEscape(kv string) string {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return shQuote(kv)
	}
	return kv[:i] + "=" + shQuote(kv[i+1:])
}
