package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

var (
	holdStop       bool
	holdForeground bool
	holdIdle       time.Duration
)

var holdCmd = &cobra.Command{
	Use:   "hold",
	Short: "Hold a persistent connection so other tn commands skip the dial",
	Long: `Dials the host once, detaches into the background, and keeps the SSH
connection open behind a Unix socket (~/.tn/hold-<host>.sock, mode 0600).
Non-interactive "tn exec", "tn read", "tn write", "tn ls", "tn push" and
"tn pull" start this hold automatically when needed; running "tn hold"
explicitly prewarms it. Concurrent invocations open independent SSH channels
on the same connection, avoiding duplicate jump + target handshakes.

The held path removes connection setup, not the remote SSH session, shell, and
process startup paid by every exec. Output, exit codes, and flags stay the same.

Commands not routed through the hold (they dial directly, by design):
"tn shell", "tn proxy", "tn mirror", "tn bench", "tn doctor",
"tn upload-key". If no hold is running — or the daemon died, or the
socket is stale — every command silently falls back to a direct dial,
so holding is purely an optimization, never a requirement.

The daemon exits on its own after --idle (default 30m) with no
operations, when the SSH connection drops, or on "tn hold --stop". A
stale socket left by a killed daemon is detected and cleaned up by the
next tn command. Logs go to ~/.tn/hold-<host>.log.

The background dial cannot prompt: it needs non-interactive auth
(identityFile, agent, or passwordCmd) and an already-pinned host key.
If "tn hold" fails, run "tn status" once — interactively — to pin the
key and verify auth, then retry.`,
	Example: `  tn hold                    # hold the default host
  tn hold -H prod --idle 2h  # explicit host, longer idle timeout
  tn status                  # shows held: true, dialMs: 0 (held)
  tn hold --stop             # tear the held connection down`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !holdSupported {
			return errors.New("tn hold is not supported on this platform — commands dial directly")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		host, err := cfg.Resolve(flagHost)
		if err != nil {
			return err
		}
		if holdStop {
			return withHoldLock(host, func() error { return stopHold(host) })
		}
		if holdForeground {
			return runHoldDaemon(host)
		}
		return ensureHold(host, false)
	},
}

func init() {
	holdCmd.Flags().BoolVar(&holdStop, "stop", false, "stop the held connection for the host")
	holdCmd.Flags().DurationVar(&holdIdle, "idle", 30*time.Minute, "shut down after this long without an operation")
	holdCmd.Flags().BoolVar(&holdForeground, "foreground", false, "run the daemon in the foreground (used by the detach; handy for debugging)")
}

// holdSocketPath returns the per-host Unix socket path under $TN_HOME.
func holdSocketPath(hostName string) string {
	return filepath.Join(config.Home(), "hold-"+hostName+".sock")
}

func holdLogPath(hostName string) string {
	return filepath.Join(config.Home(), "hold-"+hostName+".log")
}

// holdProbe connects to sock and reads the daemon's hello without mutating
// socket state.
func holdProbe(sock string) (*holdHello, error) {
	conn, hello, err := holdAttach(sock)
	if err != nil {
		return nil, err
	}
	conn.Close()
	return hello, nil
}

// holdAttach dials sock and reads the hello frame, leaving the connection
// open for a request. It is observation-only: callers that own startup or
// cleanup decide whether a failed socket should be removed.
func holdAttach(sock string) (net.Conn, *holdHello, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	t, payload, err := newFrameConn(conn).readFrame()
	if err != nil || t != frameHello {
		conn.Close()
		return nil, nil, fmt.Errorf("hold socket %s: bad hello: %v", sock, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	hello := &holdHello{}
	if err := json.Unmarshal(payload, hello); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if hello.Version != holdProtoVersion {
		if p, err := os.FindProcess(hello.Pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
		conn.Close()
		return nil, nil, fmt.Errorf("stopped incompatible hold daemon (protocol v%d); retry the command", hello.Version)
	}
	return conn, hello, nil
}

// stopHold asks a running daemon to shut down. Idempotent when no daemon is
// available.
func stopHold(host *config.Host) error {
	sock := holdSocketPath(host.Name)
	conn, hello, err := holdAttach(sock)
	if err != nil {
		fmt.Printf("no held connection for %s\n", host.Name)
		return nil
	}
	defer conn.Close()
	_, err = holdRoundTrip(newFrameConn(conn), holdRequest{Op: "stop"}, nil, io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("stop hold: %w", err)
	}
	fmt.Printf("stopped hold for %s (pid %d)\n", host.Name, hello.Pid)
	return nil
}

// prepareHoldSocket removes only paths proven stale. Timeouts, permission
// failures, and protocol errors may belong to a live daemon and are preserved.
func prepareHoldSocket(sock string, probeErr error) error {
	info, err := os.Lstat(sock)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || errors.Is(probeErr, syscall.ECONNREFUSED) {
		return os.Remove(sock)
	}
	return fmt.Errorf("hold socket %s exists but is not responding: %w", sock, probeErr)
}

// runHoldDaemon dials the host and serves the hold socket until idle
// timeout, --stop, SIGTERM, or SSH connection death.
func runHoldDaemon(host *config.Host) error {
	sock := holdSocketPath(host.Name)
	if _, err := holdProbe(sock); err == nil {
		return fmt.Errorf("already holding %s (socket %s)", host.Name, sock)
	} else if err := prepareHoldSocket(sock, err); err != nil {
		return err
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
	defer cancel()
	c, err := sshx.Dial(ctx, host, currentPolicy())
	if err != nil {
		return err
	}
	dialMs := time.Since(start).Milliseconds()

	ln, err := net.Listen("unix", sock)
	if err != nil {
		c.Close()
		return err
	}
	_ = os.Chmod(sock, 0o600)

	d := newHoldDaemon(host, c, dialMs, holdIdle)
	d.logf("holding %s (%s) — socket %s, dial %dms, idle timeout %s", host.Name, host.Target.Addr, sock, dialMs, holdIdle)
	return d.run(ln)
}

// holdDaemon serves operations over one held sshx.Client. Each socket
// connection is one operation; each operation opens its own SSH channel, so
// concurrent invocations multiplex safely.
type holdDaemon struct {
	host   *config.Host
	client *sshx.Client
	dialMs int64
	since  time.Time
	idle   time.Duration

	ops    atomic.Uint64
	active atomic.Int64
	last   atomic.Int64 // UnixNano of last op start/finish

	sftpMu sync.Mutex
	sftpc  *sftp.Client

	sem chan struct{} // limits concurrent ops to prevent resource exhaustion

	ln       net.Listener
	stopOnce sync.Once
	done     chan struct{}
}

func newHoldDaemon(host *config.Host, c *sshx.Client, dialMs int64, idle time.Duration) *holdDaemon {
	d := &holdDaemon{
		host:   host,
		client: c,
		dialMs: dialMs,
		since:  time.Now(),
		idle:   idle,
		sem:    make(chan struct{}, 64),
		done:   make(chan struct{}),
	}
	d.touch()
	return d
}

func (d *holdDaemon) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("2006-01-02 15:04:05")+" "+format+"\n", args...)
}

func (d *holdDaemon) touch() { d.last.Store(time.Now().UnixNano()) }

// run serves ln until shutdown. Closing ln unlinks the socket (Go's
// UnixListener unlinks on Close), so a clean exit leaves no stale file.
func (d *holdDaemon) run(ln net.Listener) error {
	d.ln = ln

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case s := <-sigCh:
			d.shutdown(fmt.Sprintf("signal %v", s))
		case <-d.done:
		}
	}()
	go func() {
		_ = d.client.SSH().Wait()
		d.shutdown("ssh connection closed")
	}()
	go d.idleWatch()

	for {
		conn, err := ln.Accept()
		if err != nil {
			<-d.done // shutdown in flight; wait for cleanup to finish
			return nil
		}
		go d.serveConn(conn)
	}
}

func (d *holdDaemon) shutdown(reason string) {
	d.stopOnce.Do(func() {
		d.logf("shutting down: %s", reason)
		if d.ln != nil {
			_ = d.ln.Close()
		}
		_ = d.client.Close()
		close(d.done)
	})
}

// idleWatch shuts the daemon down once no op has run for d.idle. In-flight
// operations (active > 0) always defer the timeout.
func (d *holdDaemon) idleWatch() {
	tick := max(min(d.idle/4, 10*time.Second), 10*time.Millisecond)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-t.C:
			if d.active.Load() == 0 && time.Since(time.Unix(0, d.last.Load())) >= d.idle {
				d.shutdown(fmt.Sprintf("idle for %s", d.idle))
				return
			}
		}
	}
}

// serveConn handles one connection = one operation.
func (d *holdDaemon) serveConn(nc net.Conn) {
	defer nc.Close()
	fc := newFrameConn(nc)
	if err := fc.writeJSON(frameHello, holdHello{
		Version: holdProtoVersion,
		Host:    d.host.Name,
		Target:  d.host.Target.Addr,
		Config:  holdConfigID(d.host, currentPolicy()),
		Pid:     os.Getpid(),
		DialMs:  d.dialMs,
	}); err != nil {
		return
	}
	// Probe connections read the hello and hang up; give real clients 10s
	// to send their request.
	_ = nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	t, payload, err := fc.readFrame()
	if err != nil || t != frameReq {
		return
	}
	_ = nc.SetReadDeadline(time.Time{})
	var req holdRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = fc.writeJSON(frameExit, holdExit{Code: 1, Err: "bad request: " + err.Error()})
		return
	}

	// Limit concurrent ops to prevent goroutine/resource exhaustion under load.
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	default:
		_ = fc.writeJSON(frameExit, holdExit{Code: 1, Err: "hold daemon: too many concurrent operations, retry"})
		return
	}

	d.active.Add(1)
	d.touch()
	defer func() { d.active.Add(-1); d.touch() }()
	d.ops.Add(1)

	code, opErr := d.dispatch(fc, req)
	if opErr != nil && code == 0 {
		code = 1
	}
	exit := holdExit{Code: code}
	if opErr != nil {
		exit.Err = opErr.Error()
	}
	_ = fc.writeJSON(frameExit, exit)
	if req.Op == "stop" {
		d.shutdown("tn hold --stop")
	}
}

func (d *holdDaemon) dispatch(fc *frameConn, req holdRequest) (int, error) {
	switch req.Op {
	case "exec":
		return d.opExec(fc, req)
	case "read":
		return 0, d.withSFTP(func(sc *sftp.Client) error {
			return readFile(sc, req.Path, fc.stream(frameStdout), req.JSON)
		})
	case "write":
		return 0, d.opWrite(fc, req)
	case "ls":
		return 0, d.withSFTP(func(sc *sftp.Client) error {
			return list(sc, req.Path, fc.stream(frameStdout), req.Long, req.JSON)
		})
	case "push":
		if !filepath.IsAbs(req.Local) {
			return 0, fmt.Errorf("push through hold needs an absolute local path, got %q", req.Local)
		}
		return 0, d.withSFTP(func(sc *sftp.Client) error { return push(sc, req.Local, req.Remote) })
	case "pull":
		if !filepath.IsAbs(req.Local) {
			return 0, fmt.Errorf("pull through hold needs an absolute local path, got %q", req.Local)
		}
		return 0, d.withSFTP(func(sc *sftp.Client) error { return pull(sc, req.Remote, req.Local) })
	case "info":
		return 0, d.opInfo(fc)
	case "stop":
		return 0, nil
	default:
		return 0, fmt.Errorf("hold daemon does not proxy op %q — this command dials directly", req.Op)
	}
}

// opExec runs the composed command on a fresh session, bridging the
// client's stdin/signal frames in and stdout/stderr frames out.
func (d *holdDaemon) opExec(fc *frameConn, req holdRequest) (int, error) {
	sess, err := d.client.SSH().NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()

	stdinR, stdinW := io.Pipe()
	sess.Stdin = stdinR
	sess.Stdout = fc.stream(frameStdout)
	sess.Stderr = fc.stream(frameStderr)

	go func() {
		defer stdinW.Close()
		for {
			t, p, err := fc.readFrame()
			if err != nil {
				stdinW.CloseWithError(err)
				return
			}
			switch t {
			case frameStdin:
				if _, err := stdinW.Write(p); err != nil {
					return
				}
			case frameStdinEOF:
				return
			case frameSignal:
				_ = sess.Signal(ssh.Signal(string(p)))
			}
		}
	}()

	code, err := exitCodeOf(sess.Run(buildExecCommand(req.Args, req.Env, req.Cwd, req.Proxy)))
	stdinR.Close() // unblock a stdin bridge stuck writing to a finished session
	return code, err
}

// opWrite streams the client's stdin frames into an atomic remote write.
func (d *holdDaemon) opWrite(fc *frameConn, req holdRequest) error {
	pr, pw := io.Pipe()
	go func() {
		for {
			t, p, err := fc.readFrame()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			switch t {
			case frameStdin:
				if _, err := pw.Write(p); err != nil {
					return
				}
			case frameStdinEOF:
				pw.Close()
				return
			}
		}
	}()
	err := d.withSFTP(func(sc *sftp.Client) error { return writeFile(sc, req.Path, pr) })
	pr.CloseWithError(err) // unblock the bridge if the write failed mid-stream
	return err
}

// opInfo reports daemon state plus a fresh ping and remote summary.
func (d *holdDaemon) opInfo(fc *frameConn) error {
	info := holdInfo{
		Host:        d.host.Name,
		Target:      d.host.Target.Addr,
		HasJump:     d.host.Jump != nil,
		Pid:         os.Getpid(),
		DialMs:      d.dialMs,
		HeldSince:   d.since.Format(time.RFC3339),
		OpsServed:   d.ops.Load(),
		IdleTimeout: d.idle.String(),
	}
	probe := probeStatus(d.client)
	info.PingMs = probe.PingMs
	info.PingError = probe.PingError
	info.Remote = probe.Remote
	info.RemoteError = probe.RemoteError
	return json.NewEncoder(fc.stream(frameStdout)).Encode(info)
}

// withSFTP runs f against a lazily-created shared SFTP client. On a lost
// connection the cached client is dropped so the next op reopens the
// subsystem; f itself is never retried (it may already have emitted output).
func (d *holdDaemon) withSFTP(f func(*sftp.Client) error) error {
	d.sftpMu.Lock()
	if d.sftpc == nil {
		sc, err := newSFTP(d.client)
		if err != nil {
			d.sftpMu.Unlock()
			return err
		}
		d.sftpc = sc
	}
	sc := d.sftpc
	d.sftpMu.Unlock()

	err := f(sc)
	if errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		d.sftpMu.Lock()
		if d.sftpc == sc {
			_ = sc.Close()
			d.sftpc = nil
		}
		d.sftpMu.Unlock()
	}
	return err
}
