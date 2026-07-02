package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show connection health: dial time, RTT, remote uname",
	Long: `Dials the host once, runs a minimal session to read uname/uptime/df,
and prints the result. The plain output is one "key: value" line per
field — easy to grep, easy for an agent to read without parsing JSON.

Fields:
  host        alias from ~/.tn/config.yaml
  target      target addr that was dialled
  hasJump     whether a jump host is configured
  held        true if a "tn hold" daemon served this request (no dial)
  dialMs      time to TCP+SSH+jump handshake (ms); "0 (held)" via a hold
  pingMs      one-shot keepalive RTT (ms) — ~handshake-free per-call cost
  remote      uname -srm | uptime | df -h $HOME (one line each)
  ok          true if dial+ping+session all succeeded
  error       on failure: classified message ("auth failed — try
              tn upload-key", "VPN down?", etc.)

When held, extra fields report the daemon: heldPid, heldSince, opsServed,
idleTimeout, and heldDialMs (the dial paid once when the hold started).

Exits 0 even on failure — read "ok:" or use --json for a hard signal.
For multi-host pass/fail in CI, use "tn doctor".`,
	Example: `  tn status
  tn status -H prod
  tn status --json | jq '.ok'`,
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

		if info, ok := heldInfo(); ok {
			report := map[string]any{
				"host":        host.Name,
				"target":      info.Target,
				"hasJump":     info.HasJump,
				"held":        true,
				"pingMs":      info.PingMs,
				"heldDialMs":  info.DialMs,
				"heldSince":   info.HeldSince,
				"heldPid":     info.Pid,
				"opsServed":   info.OpsServed,
				"idleTimeout": info.IdleTimeout,
				"remote":      info.Remote,
				"clientVer":   "tn 0.1",
				"goVersion":   runtime.Version(),
				"ok":          true,
			}
			if flagJSON {
				report["dialMs"] = 0
			} else {
				report["dialMs"] = "0 (held)"
			}
			return emit(report, true)
		}

		dialStart := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
		defer cancel()
		c, err := sshx.Dial(ctx, host, policy)
		dialMs := time.Since(dialStart).Milliseconds()
		report := map[string]any{
			"host":      host.Name,
			"target":    host.Target.Addr,
			"hasJump":   host.Jump != nil,
			"held":      false,
			"dialMs":    dialMs,
			"clientVer": "tn 0.1",
			"goVersion": runtime.Version(),
		}
		if err != nil {
			report["ok"] = false
			report["error"] = err.Error()
			return emit(report, false)
		}
		defer c.Close()

		rtt, perr := c.Ping()
		report["pingMs"] = rtt.Milliseconds()
		if perr != nil {
			report["pingError"] = perr.Error()
		}

		// Remote info: uname + uptime + free disk on $HOME, single round trip.
		sess, err := c.SSH().NewSession()
		if err == nil {
			out, _ := sess.CombinedOutput(remoteInfoCmd)
			sess.Close()
			report["remote"] = strings.TrimSpace(string(out))
		}
		report["ok"] = true
		return emit(report, true)
	},
}

// remoteInfoCmd summarizes the remote in one round trip. Shared by the
// direct status path and the hold daemon's info op.
const remoteInfoCmd = `uname -srm 2>/dev/null; uptime 2>/dev/null; df -h "$HOME" 2>/dev/null | tail -1`

func emit(report map[string]any, _ bool) error {
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	order := []string{"host", "target", "hasJump", "held", "dialMs", "pingMs", "heldDialMs", "heldSince", "heldPid", "opsServed", "idleTimeout", "remote", "clientVer", "goVersion", "ok", "error", "pingError"}
	for _, k := range order {
		v, ok := report[k]
		if !ok {
			continue
		}
		fmt.Printf("%-10s %v\n", k+":", v)
	}
	return nil
}
