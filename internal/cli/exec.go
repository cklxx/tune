package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cklxx/tune/internal/sshx"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

var (
	execProxy bool
	execEnv   []string
	execCwd   string
	execIdle  = 5 * time.Minute
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
to be running in another shell.

Non-interactive calls automatically start and reuse the per-host held
connection. Interactive calls reuse an existing hold or dial directly (see
"tn hold --help").`,
	Args:               cobra.MinimumNArgs(1),
	DisableFlagParsing: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		req := holdRequest{Op: "exec", Args: args, Env: execEnv, Cwd: execCwd, Proxy: execProxy}
		code, ok, host, err := tryHeld(req, os.Stdin, os.Stdout, os.Stderr)
		if ok {
			return heldResult(code, err)
		}
		c, _, err := connectResolved(host)
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
	execCmd.Flags().DurationVar(&execIdle, "idle", 5*time.Minute, "abort if no remote output for this long (heartbeat every 15s)")
}

// buildExecCommand composes the final remote shell command from argv plus
// the --cwd/--env/--proxy modifiers. Shared by the direct path and the hold
// daemon so both run byte-identical command lines.
func buildExecCommand(args, env []string, cwd string, proxy bool) string {
	prefix := ""
	if cwd != "" {
		prefix += fmt.Sprintf("cd %s && ", shQuote(cwd))
	}
	for _, kv := range env {
		prefix += "export " + envEscape(kv) + "; "
	}
	if proxy {
		// Source ~/.tn/proxy.env (written by `tn proxy`), but first verify
		// the listener is alive — a stale env file from a crashed `tn
		// proxy` would otherwise silently route through nothing.
		prefix += proxyEnvPrefix
	}
	return prefix + joinShell(args)
}

// exitCodeOf maps a session Run error to (remote exit code, remaining
// error). A remote nonzero exit is a code, not an error; EOF on a clean
// channel teardown counts as success.
func exitCodeOf(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus(), nil
	}
	if errors.Is(err, io.EOF) {
		return 0, nil
	}
	return 0, err
}

// runExec executes args as a single shell command on the remote, piping
// local stdio. Returns the same exit code the remote process exited with.
func runExec(c *sshx.Client, args, env []string, cwd string, proxy bool) error {
	sess, err := c.SSH().NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

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

	code, err := exitCodeOf(sess.Run(buildExecCommand(args, env, cwd, proxy)))
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// proxyEnvPrefix is shell injected before the user's command when --proxy is
// set. It reads ~/.tn/proxy.env (dropped by `tn proxy --write-env`), parses
// the listen port out of the marker comment, probes that port via bash's
// /dev/tcp pseudo-device, and only then sources the env. If the listener is
// dead or the file is missing it prints a one-line error and exits with
// status 2 so the caller can distinguish from the inner command's exit code.
//
// We use a heredoc-free composition because the whole command is itself
// shell-quoted by the calling shell on the remote.
const proxyEnvPrefix = `__tn_env="$HOME/.tn/proxy.env"; ` +
	`if [ -f "$__tn_env" ]; then ` +
	`__tn_port=$(awk -F= '/^# tn-proxy-port=/ {print $2; exit}' "$__tn_env"); ` +
	`if [ -z "$__tn_port" ]; then __tn_port=$(awk -F: '/socks5h:\/\/127\.0\.0\.1:/ {sub(/.*127\.0\.0\.1:/,""); sub(/[^0-9].*/,""); print; exit}' "$__tn_env"); fi; ` +
	`if [ -n "$__tn_port" ] && (exec 3<>/dev/tcp/127.0.0.1/$__tn_port) 2>/dev/null; then ` +
	`exec 3<&- 3>&-; . "$__tn_env"; ` +
	`else ` +
	`echo "tn exec --proxy: stale or dead proxy.env (no listener on port $__tn_port) — start 'tn proxy' first" 1>&2; ` +
	`exit 2; ` +
	`fi; ` +
	`else ` +
	`echo "tn exec --proxy: no ~/.tn/proxy.env on remote — start 'tn proxy' first" 1>&2; ` +
	`exit 2; ` +
	`fi; `

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
