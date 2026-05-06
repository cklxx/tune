package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshtest"
	"github.com/cklxx/tune/internal/sshx"
	"golang.org/x/crypto/ssh"
)

// runBench drives the bench command's logic directly (without cobra) so we
// can capture the report and assert on it. It intentionally mirrors what
// benchCmd's RunE does — keep them in sync if you change one.
func runBench(t *testing.T, c *sshx.Client, pings, execs int, payload int64) map[string]any {
	t.Helper()
	prevPings, prevExecs, prevPayload, prevJSON := benchPings, benchExecRuns, benchPayload, flagJSON
	benchPings, benchExecRuns, benchPayload, flagJSON = pings, execs, payload, true
	t.Cleanup(func() {
		benchPings, benchExecRuns, benchPayload, flagJSON = prevPings, prevExecs, prevPayload, prevJSON
	})

	// We can't call the cobra RunE directly without going through the CLI
	// pipeline. Instead, we duplicate the small body of work it performs.
	report := map[string]any{}

	if pings > 0 {
		var sum time.Duration
		var ok int
		for i := 0; i < pings; i++ {
			rtt, err := c.Ping()
			if err != nil {
				t.Fatalf("ping: %v", err)
			}
			sum += rtt
			ok++
		}
		report["pingCount"] = ok
	}
	if execs > 0 {
		var ok int
		for i := 0; i < execs; i++ {
			sess, err := c.SSH().NewSession()
			if err != nil {
				t.Fatalf("session: %v", err)
			}
			if err := sess.Run(":"); err == nil {
				ok++
			}
			sess.Close()
		}
		report["execCount"] = ok
	}
	if payload > 0 {
		sess, err := c.SSH().NewSession()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := sess.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Start("dump"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(stdin, zeroReader{}, payload); err != nil {
			t.Fatal(err)
		}
		stdin.Close()
		if err := sess.Wait(); err != nil {
			t.Fatalf("wait: %v", err)
		}
		report["payloadBytes"] = payload
	}

	// Re-encode/decode just to make sure the JSON path is exercised too.
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(report)
	out := map[string]any{}
	_ = json.NewDecoder(&buf).Decode(&out)
	return out
}

// TestBenchEndToEnd exercises ping + exec + throughput against an in-process
// SSH server with a custom exec handler that drains stdin (like cat).
func TestBenchEndToEnd(t *testing.T) {
	kp := sshtest.GenKey(t)

	// Custom exec handler: read all of stdin, exit 0. This makes the
	// throughput phase work without a real "cat".
	srv := sshtest.Start(t, sshtest.Options{
		AllowedKey: kp.PublicKey,
		AllowExec:  true,
		ExecHandler: func(cmd string, ch ssh.Channel) int {
			_, _ = io.Copy(io.Discard, ch)
			return 0
		},
	})

	host := &config.Host{
		Target:     config.Hop{Addr: srv.Addr, User: "alice", IdentityFile: kp.Path},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sshx.Dial(ctx, host, sshx.PolicyInsecure)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	report := runBench(t, c, 5, 3, 64*1024)

	// Asserts on the (decoded JSON) report — Number for ints when round-tripped.
	if got := mustNum(t, report, "pingCount"); got != 5 {
		t.Errorf("pingCount = %v", got)
	}
	if got := mustNum(t, report, "execCount"); got != 3 {
		t.Errorf("execCount = %v", got)
	}
	if got := mustNum(t, report, "payloadBytes"); got != 64*1024 {
		t.Errorf("payloadBytes = %v", got)
	}
}

func mustNum(t *testing.T, m map[string]any, k string) int64 {
	t.Helper()
	v, ok := m[k]
	if !ok {
		t.Fatalf("key %q missing in %+v", k, m)
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := strconv.ParseInt(n.String(), 10, 64)
		return i
	}
	t.Fatalf("key %q has unexpected type %T", k, v)
	return 0
}
