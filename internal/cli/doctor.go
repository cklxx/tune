package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/spf13/cobra"
)

var (
	doctorParallel int
	doctorTimeout  time.Duration
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check every configured host: dial, auth, ping",
	Long: `Probes every host in ~/.tn/config.yaml in parallel with a short
timeout and prints whether it's reachable and how fast. Useful to spot
broken jumpboxes, expired creds, or DNS issues at a glance.

Exits non-zero if any host failed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if len(cfg.Hosts) == 0 {
			return fmt.Errorf("no hosts configured (try `tn init`)")
		}
		policy := sshx.PolicyTOFU
		if flagInsecure {
			policy = sshx.PolicyInsecure
		}

		names := make([]string, 0, len(cfg.Hosts))
		for n := range cfg.Hosts {
			names = append(names, n)
		}
		sort.Strings(names)

		results := make([]hostResult, len(names))

		sem := make(chan struct{}, doctorParallel)
		var wg sync.WaitGroup
		for i, n := range names {
			i, n := i, n
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				results[i] = probeHost(cfg.Hosts[n], policy, doctorTimeout)
			}()
		}
		wg.Wait()

		anyFail := false
		for _, r := range results {
			if !r.OK {
				anyFail = true
			}
		}

		if flagJSON {
			_ = json.NewEncoder(os.Stdout).Encode(results)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-30s %-8s %-8s %s\n", "host", "target", "dial", "ping", "status")
			for _, r := range results {
				dialMs := fmt.Sprintf("%dms", r.DialMs)
				pingMs := "-"
				if r.OK {
					pingMs = fmt.Sprintf("%dms", r.PingMs)
				}
				status := "OK"
				if !r.OK {
					status = "FAIL: " + r.Error
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-30s %-8s %-8s %s\n", r.Host, r.Target, dialMs, pingMs, status)
			}
		}

		if anyFail {
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	doctorCmd.Flags().IntVar(&doctorParallel, "parallel", 4, "max concurrent probes")
	doctorCmd.Flags().DurationVar(&doctorTimeout, "timeout", 8*time.Second, "per-host dial timeout")
}

type hostResult struct {
	Host   string `json:"host"`
	Target string `json:"target"`
	Jump   string `json:"jump,omitempty"`
	OK     bool   `json:"ok"`
	DialMs int64  `json:"dialMs"`
	PingMs int64  `json:"pingMs,omitempty"`
	Error  string `json:"error,omitempty"`
}

func probeHost(h *config.Host, policy sshx.HostKeyPolicy, timeout time.Duration) hostResult {
	r := hostResult{Host: h.Name, Target: h.Target.Addr}
	if h.Jump != nil {
		r.Jump = h.Jump.Addr
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	c, err := sshx.Dial(ctx, h, policy)
	r.DialMs = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer c.Close()

	rtt, perr := c.Ping()
	if perr != nil {
		r.Error = "ping: " + perr.Error()
		return r
	}
	r.PingMs = rtt.Milliseconds()
	r.OK = true
	return r
}
