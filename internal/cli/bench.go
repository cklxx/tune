package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/spf13/cobra"
)

var (
	benchPings    int
	benchExecRuns int
	benchPayload  int64
)

var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Measure connection latency and throughput",
	Long: `Measures three things against the configured host:

  1. Cold dial cost (TCP + SSH handshake, including jump if any).
  2. RTT distribution over N keepalive global requests.
  3. Time to start + run a no-op shell command N times (a useful proxy for
     the per-call cost of "tn exec" without a daemon).
  4. Single-stream upload throughput by pumping --payload bytes from
     /dev/zero through "cat > /dev/null".

The numbers tell you whether you need a daemon. Sub-100ms exec on a 50ms-RTT
link is normal; if it's 500ms+ you have a network or auth-renegotiation
problem.`,
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

		report := map[string]any{"host": host.Name}

		dialStart := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
		defer cancel()
		c, err := sshx.Dial(ctx, host, policy)
		report["dialMs"] = time.Since(dialStart).Milliseconds()
		if err != nil {
			report["error"] = err.Error()
			return emitBench(report)
		}
		defer c.Close()

		// 1. Pings.
		if benchPings > 0 {
			pings := make([]int64, 0, benchPings)
			var sum, mn, mx time.Duration
			mn = time.Hour
			for i := 0; i < benchPings; i++ {
				rtt, err := c.Ping()
				if err != nil {
					report["pingError"] = err.Error()
					break
				}
				pings = append(pings, rtt.Microseconds())
				sum += rtt
				if rtt < mn {
					mn = rtt
				}
				if rtt > mx {
					mx = rtt
				}
			}
			if len(pings) > 0 {
				report["pingCount"] = len(pings)
				report["pingMinMs"] = float64(mn.Microseconds()) / 1000
				report["pingMaxMs"] = float64(mx.Microseconds()) / 1000
				report["pingAvgMs"] = float64(sum.Microseconds()) / 1000 / float64(len(pings))
			}
		}

		// 2. Exec turnaround.
		if benchExecRuns > 0 {
			var sum time.Duration
			var ok int
			for i := 0; i < benchExecRuns; i++ {
				sess, err := c.SSH().NewSession()
				if err != nil {
					report["execError"] = err.Error()
					break
				}
				start := time.Now()
				err = sess.Run(":")
				sess.Close()
				if err == nil {
					sum += time.Since(start)
					ok++
				}
			}
			if ok > 0 {
				report["execCount"] = ok
				report["execAvgMs"] = float64(sum.Microseconds()) / 1000 / float64(ok)
			}
		}

		// 3. Throughput: pipe N bytes through `cat >/dev/null`. Single
		// stream — does not measure parallelism.
		if benchPayload > 0 {
			sess, err := c.SSH().NewSession()
			if err != nil {
				report["throughputError"] = err.Error()
				return emitBench(report)
			}
			stdin, err := sess.StdinPipe()
			if err != nil {
				sess.Close()
				report["throughputError"] = err.Error()
				return emitBench(report)
			}
			if err := sess.Start("cat >/dev/null"); err != nil {
				sess.Close()
				report["throughputError"] = err.Error()
				return emitBench(report)
			}
			start := time.Now()
			_, copyErr := io.CopyN(stdin, zeroReader{}, benchPayload)
			stdin.Close()
			waitErr := sess.Wait()
			elapsed := time.Since(start)
			sess.Close()
			if copyErr != nil || waitErr != nil {
				report["throughputError"] = fmt.Sprintf("copy=%v wait=%v", copyErr, waitErr)
			} else {
				report["payloadBytes"] = benchPayload
				report["throughputMs"] = elapsed.Milliseconds()
				if elapsed > 0 {
					mibPerSec := float64(benchPayload) / (1 << 20) / elapsed.Seconds()
					report["throughputMiBs"] = mibPerSec
				}
			}
		}

		return emitBench(report)
	},
}

func init() {
	benchCmd.Flags().IntVar(&benchPings, "pings", 10, "number of keepalive pings (0 = skip)")
	benchCmd.Flags().IntVar(&benchExecRuns, "execs", 5, "number of no-op exec runs (0 = skip)")
	benchCmd.Flags().Int64Var(&benchPayload, "payload", 1<<20, "bytes to push through cat >/dev/null for throughput (0 = skip)")
}

func emitBench(report map[string]any) error {
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	order := []string{
		"host", "dialMs",
		"pingCount", "pingMinMs", "pingAvgMs", "pingMaxMs",
		"execCount", "execAvgMs",
		"payloadBytes", "throughputMs", "throughputMiBs",
		"error", "pingError", "execError", "throughputError",
	}
	for _, k := range order {
		v, ok := report[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			fmt.Printf("%-16s %.3f\n", k+":", t)
		default:
			fmt.Printf("%-16s %v\n", k+":", v)
		}
	}
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
