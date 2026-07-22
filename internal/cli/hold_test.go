package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshtest"
	"github.com/cklxx/tune/internal/sshx"
	"golang.org/x/crypto/ssh"
)

// shortTempDir returns a temp dir outside t.TempDir(): Unix socket paths must
// fit in sun_path (~104 bytes on macOS) and t.TempDir can exceed that.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tnh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// dialHost dials the test server and returns the sshx client + host config.
func dialHost(t *testing.T, srv *sshtest.Server, kp sshtest.KeyPair) (*sshx.Client, *config.Host) {
	t.Helper()
	host := &config.Host{
		Name:       "t",
		Target:     config.Hop{Addr: srv.Addr, User: "alice", IdentityFile: kp.Path},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := sshx.Dial(ctx, host, sshx.PolicyInsecure)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, host
}

// startDaemon runs a hold daemon over the given client on a fresh socket.
func startDaemon(t *testing.T, host *config.Host, c *sshx.Client, idle time.Duration, sock string) *holdDaemon {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	d := newHoldDaemon(host, c, 42, idle)
	go func() { _ = d.run(ln) }()
	t.Cleanup(func() { d.shutdown("test cleanup") })
	return d
}

// heldOp runs one op through the daemon socket, capturing stdout/stderr.
func heldOp(t *testing.T, sock string, req holdRequest, stdin io.Reader) (string, string, int, error) {
	t.Helper()
	conn, _, err := holdAttach(sock)
	if err != nil {
		t.Fatalf("attach %s: %v", sock, err)
	}
	defer conn.Close()
	var out, errw bytes.Buffer
	code, err := holdRoundTrip(newFrameConn(conn), req, stdin, &out, &errw)
	return out.String(), errw.String(), code, err
}

// TestHoldExecParity: exec through the daemon returns byte-identical
// stdout/stderr and the same exit code as a direct SSH session.
func TestHoldExecParity(t *testing.T) {
	kp := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{
		AllowedKey: kp.PublicKey,
		AllowExec:  true,
		ExecHandler: func(cmd string, ch ssh.Channel) int {
			io.WriteString(ch, "out:"+cmd+"\n")
			data, _ := io.ReadAll(ch) // echo stdin back
			_, _ = ch.Write(data)
			io.WriteString(ch.Stderr(), "err:"+cmd+"\n")
			return 7
		},
	})

	// Direct reference run.
	direct, _ := dialHost(t, srv, kp)
	sess, err := direct.SSH().NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var dOut, dErr bytes.Buffer
	sess.Stdin = strings.NewReader("ping")
	sess.Stdout, sess.Stderr = &dOut, &dErr
	dCode, err := exitCodeOf(sess.Run(buildExecCommand([]string{"hostname"}, nil, "", false)))
	sess.Close()
	if err != nil {
		t.Fatalf("direct run: %v", err)
	}

	// Held run, same command, same stdin.
	held, host := dialHost(t, srv, kp)
	sock := filepath.Join(shortTempDir(t), "h.sock")
	startDaemon(t, host, held, time.Minute, sock)
	hOut, hErr, hCode, err := heldOp(t, sock, holdRequest{Op: "exec", Args: []string{"hostname"}}, strings.NewReader("ping"))
	if err != nil {
		t.Fatalf("held run: %v", err)
	}

	if hOut != dOut.String() || hErr != dErr.String() || hCode != dCode {
		t.Errorf("held != direct:\n stdout %q vs %q\n stderr %q vs %q\n code %d vs %d",
			hOut, dOut.String(), hErr, dErr.String(), hCode, dCode)
	}
	if hCode != 7 {
		t.Errorf("exit code = %d, want 7", hCode)
	}
}

// TestHoldFileOps: write → read (plain + JSON) → ls through the daemon,
// verified against the SFTP root on disk.
func TestHoldFileOps(t *testing.T) {
	kp := sshtest.GenKey(t)
	root := t.TempDir()
	srv := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowSFTP: true, SFTPRoot: root})
	c, host := dialHost(t, srv, kp)
	sock := filepath.Join(shortTempDir(t), "h.sock")
	startDaemon(t, host, c, time.Minute, sock)

	// write (atomic, stdin-streamed)
	body := strings.Repeat("hold-me ", 10_000) // > one chunk
	_, _, code, err := heldOp(t, sock, holdRequest{Op: "write", Path: "f.txt"}, strings.NewReader(body))
	if err != nil || code != 0 {
		t.Fatalf("write: code=%d err=%v", code, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "f.txt"))
	if err != nil || string(onDisk) != body {
		t.Fatalf("write landed wrong: err=%v len=%d want %d", err, len(onDisk), len(body))
	}
	if _, err := os.Stat(filepath.Join(root, "f.txt.tn-tmp")); !os.IsNotExist(err) {
		t.Errorf("tmp file leaked: %v", err)
	}

	// read, plain
	out, _, code, err := heldOp(t, sock, holdRequest{Op: "read", Path: "f.txt"}, nil)
	if err != nil || code != 0 || out != body {
		t.Fatalf("read: code=%d err=%v len=%d", code, err, len(out))
	}

	// read, JSON frame
	out, _, code, err = heldOp(t, sock, holdRequest{Op: "read", Path: "f.txt", JSON: true}, nil)
	if err != nil || code != 0 {
		t.Fatalf("read --json: code=%d err=%v", code, err)
	}
	var frame struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &frame); err != nil || frame.Content != body {
		t.Fatalf("read --json: %v %q", err, frame.Path)
	}

	// ls
	out, _, code, err = heldOp(t, sock, holdRequest{Op: "ls", Path: "."}, nil)
	if err != nil || code != 0 || !strings.Contains(out, "f.txt") {
		t.Fatalf("ls: code=%d err=%v out=%q", code, err, out)
	}

	// read of a missing file → nonzero code + error, nothing on stdout
	out, _, code, err = heldOp(t, sock, holdRequest{Op: "read", Path: "nope.txt"}, nil)
	if err == nil || code == 0 || out != "" {
		t.Fatalf("read missing: code=%d err=%v out=%q", code, err, out)
	}
}

// TestHoldPushPull: recursive push and pull through the daemon.
func TestHoldPushPull(t *testing.T) {
	kp := sshtest.GenKey(t)
	root := t.TempDir()
	srv := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowSFTP: true, SFTPRoot: root})
	c, host := dialHost(t, srv, kp)
	sock := filepath.Join(shortTempDir(t), "h.sock")
	startDaemon(t, host, c, time.Minute, sock)

	srcDir := t.TempDir()
	want := map[string]string{"a.txt": "hello", "sub/b.txt": "world"}
	for p, body := range want {
		full := filepath.Join(srcDir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, _, code, err := heldOp(t, sock, holdRequest{Op: "push", Local: srcDir, Remote: "up"}, nil)
	if err != nil || code != 0 {
		t.Fatalf("push: code=%d err=%v", code, err)
	}
	for p, body := range want {
		got, err := os.ReadFile(filepath.Join(root, "up", p))
		if err != nil || string(got) != body {
			t.Errorf("pushed %s = %q (%v), want %q", p, got, err, body)
		}
	}

	pullDir := filepath.Join(t.TempDir(), "down")
	_, _, code, err = heldOp(t, sock, holdRequest{Op: "pull", Remote: "up", Local: pullDir}, nil)
	if err != nil || code != 0 {
		t.Fatalf("pull: code=%d err=%v", code, err)
	}
	for p, body := range want {
		got, err := os.ReadFile(filepath.Join(pullDir, p))
		if err != nil || string(got) != body {
			t.Errorf("pulled %s = %q (%v), want %q", p, got, err, body)
		}
	}

	// Relative local paths are rejected (the daemon's cwd is not the caller's).
	_, _, code, err = heldOp(t, sock, holdRequest{Op: "push", Local: "rel/path", Remote: "up"}, nil)
	if err == nil || code == 0 {
		t.Fatalf("relative push should fail: code=%d err=%v", code, err)
	}
}

// TestHoldConcurrentExecs: concurrent invocations multiplex over the one
// held client without crosstalk.
func TestHoldConcurrentExecs(t *testing.T) {
	kp := sshtest.GenKey(t)
	const n = 8

	var active atomic.Int64
	var peak atomic.Int64
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	srv := sshtest.Start(t, sshtest.Options{
		AllowedKey: kp.PublicKey,
		AllowExec:  true,
		ExecHandler: func(cmd string, ch ssh.Channel) int {
			current := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
			}
			entered <- struct{}{}
			<-release
			_, _ = io.WriteString(ch, "EXEC: "+cmd+"\n")
			return 0
		},
	})
	c, host := dialHost(t, srv, kp)
	sock := filepath.Join(shortTempDir(t), "h.sock")
	d := startDaemon(t, host, c, time.Minute, sock)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := holdAttach(sock)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			var out, errw bytes.Buffer
			cmd := fmt.Sprintf("cmd-%d", i)
			code, err := holdRoundTrip(newFrameConn(conn), holdRequest{Op: "exec", Args: []string{cmd}}, nil, &out, &errw)
			if err != nil || code != 0 || out.String() != "EXEC: "+cmd+"\n" || errw.Len() != 0 {
				errs <- fmt.Errorf("op %d: code=%d err=%v out=%q stderr=%q", i, code, err, out.String(), errw.String())
			}
		}()
	}

	for range n {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("concurrent execs did not all reach the server barrier")
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := peak.Load(); got != n {
		t.Errorf("peak concurrent execs = %d, want %d", got, n)
	}
	if got := d.ops.Load(); got != n {
		t.Errorf("opsServed = %d, want %d", got, n)
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Errorf("SSH connections = %d, want 1", got)
	}
}

func TestHoldReusesSSHConnection(t *testing.T) {
	kp := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowExec: true})
	c, host := dialHost(t, srv, kp)
	sock := filepath.Join(shortTempDir(t), "h.sock")
	startDaemon(t, host, c, time.Minute, sock)

	for i := range 3 {
		cmd := fmt.Sprintf("cmd-%d", i)
		out, stderr, code, err := heldOp(t, sock, holdRequest{Op: "exec", Args: []string{cmd}}, nil)
		if err != nil || code != 0 || out != "EXEC: "+cmd+"\n" || stderr != "" {
			t.Fatalf("op %d: code=%d err=%v out=%q stderr=%q", i, code, err, out, stderr)
		}
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("SSH connections = %d, want 1", got)
	}
}

func TestHoldAutoStartEligibility(t *testing.T) {
	for _, op := range []string{"read", "ls", "push", "pull"} {
		if !holdAutoStartEligible(holdRequest{Op: op}, nil) {
			t.Errorf("%s should auto-start without stdin", op)
		}
	}
	if !holdAutoStartEligible(holdRequest{Op: "exec"}, strings.NewReader("")) {
		t.Error("exec with piped stdin should auto-start")
	}
	if !holdAutoStartEligible(holdRequest{Op: "write"}, strings.NewReader("body")) {
		t.Error("write with piped stdin should auto-start")
	}
	if holdAutoStartEligible(holdRequest{Op: "info"}, nil) {
		t.Error("info should not auto-start")
	}
}

// TestHoldInfoOp: the info op reports daemon state.
func TestHoldInfoOp(t *testing.T) {
	kp := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowExec: true})
	c, host := dialHost(t, srv, kp)
	sock := filepath.Join(shortTempDir(t), "h.sock")
	startDaemon(t, host, c, 45*time.Minute, sock)

	out, _, code, err := heldOp(t, sock, holdRequest{Op: "info"}, nil)
	if err != nil || code != 0 {
		t.Fatalf("info: code=%d err=%v", code, err)
	}
	var info holdInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("info json: %v\n%s", err, out)
	}
	if info.Host != "t" || info.Target != host.Target.Addr || info.DialMs != 42 || info.IdleTimeout != "45m0s" || info.OpsServed < 1 || info.Remote == "" {
		t.Errorf("info fields: %+v", info)
	}
}

// TestHoldStaleSocket: attaching to a dead socket is observation-only; the
// startup owner then removes the path after proving ECONNREFUSED.
func TestHoldStaleSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // leaves the file, nothing listening — the SIGKILL case

	_, _, probeErr := holdAttach(sock)
	if probeErr == nil {
		t.Fatal("attach to dead socket should fail")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("observation-only attach changed stale socket: %v", err)
	}
	if err := prepareHoldSocket(sock, probeErr); err != nil {
		t.Fatalf("prepare stale socket: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("stale socket not removed by startup owner: %v", err)
	}
}

func TestHoldAmbiguousSocketIsPreserved(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	probeErr := fmt.Errorf("hello timeout: %w", os.ErrDeadlineExceeded)
	if err := prepareHoldSocket(sock, probeErr); err == nil {
		t.Fatal("ambiguous live socket should block startup")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("ambiguous live socket was removed: %v", err)
	}
}

// TestHoldIdleShutdown: the daemon exits and unlinks its socket after the
// idle timeout.
func TestHoldIdleShutdown(t *testing.T) {
	kp := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowExec: true})
	c, host := dialHost(t, srv, kp)
	sock := filepath.Join(shortTempDir(t), "h.sock")
	d := startDaemon(t, host, c, 50*time.Millisecond, sock)

	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not idle out")
	}
	// Socket unlink races the done channel close by a hair; poll briefly.
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(sock); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not removed after idle shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTryHeldRouting: the transparent client path — attach when the daemon
// is up, fall back when the socket is missing or the config target moved.
func TestTryHeldRouting(t *testing.T) {
	kp := sshtest.GenKey(t)
	root := t.TempDir()
	srv := sshtest.Start(t, sshtest.Options{AllowedKey: kp.PublicKey, AllowSFTP: true, SFTPRoot: root})
	if err := os.WriteFile(filepath.Join(root, "seen.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, host := dialHost(t, srv, kp)

	tnHome := shortTempDir(t)
	t.Setenv("TN_HOME", tnHome)
	writeCfg := func(addr string) {
		cfg := fmt.Sprintf("defaultHost: t\nhosts:\n  t:\n    knownHosts: %s\n    target:\n      addr: %s\n      user: alice\n      identityFile: %s\n", host.KnownHosts, addr, host.Target.IdentityFile)
		if err := os.WriteFile(filepath.Join(tnHome, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCfg(host.Target.Addr)

	// No daemon yet → not handled; info never auto-starts one.
	var out, errw bytes.Buffer
	if _, ok, _, _ := tryHeld(holdRequest{Op: "info"}, nil, &out, &errw); ok {
		t.Fatal("tryHeld handled with no daemon up")
	}

	startDaemon(t, host, c, time.Minute, holdSocketPath("t"))

	// Daemon up → handled, output flows.
	out.Reset()
	code, ok, _, err := tryHeld(holdRequest{Op: "ls", Path: "."}, nil, &out, &errw)
	if !ok || err != nil || code != 0 || !strings.Contains(out.String(), "seen.txt") {
		t.Fatalf("tryHeld: ok=%v code=%d err=%v out=%q", ok, code, err, out.String())
	}

	// heldInfo sees the same daemon.
	if info, ok := heldInfo(); !ok || info.Host != "t" {
		t.Fatalf("heldInfo: ok=%v info=%+v", ok, info)
	}

	// Config now points elsewhere → mismatch, not handled (direct dial).
	writeCfg("198.51.100.1:22")
	if _, ok, _, _ := tryHeld(holdRequest{Op: "ls", Path: "."}, nil, &out, &errw); ok {
		t.Fatal("tryHeld should refuse a daemon holding a different target")
	}
}
