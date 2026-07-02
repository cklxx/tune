//go:build !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cklxx/tune/internal/config"
)

// holdSupported gates the whole hold path: false on Windows, where every
// command direct-dials (see hold_windows.go).
const holdSupported = true

// spawnHold re-execs "tn hold --foreground" detached (own session, stdio to
// the log file), waits for the socket to come up, and reports. The child
// performs the dial, so auth must be non-interactive — on failure the log
// tail is surfaced with a hint.
func spawnHold(host *config.Host) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := holdLogPath(host.Name)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	args := []string{"hold", "--foreground",
		"-H", host.Name,
		"--idle", holdIdle.String(),
		"--timeout", flagTimeout.String(),
	}
	if flagInsecure {
		args = append(args, "--insecure-host-key")
	}
	if flagAcceptNew {
		args = append(args, "--accept-new")
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		lf.Close()
		return err
	}
	lf.Close()

	died := make(chan struct{})
	go func() { _ = cmd.Wait(); close(died) }()

	sock := holdSocketPath(host.Name)
	deadline := time.Now().Add(flagTimeout + 5*time.Second)
	for time.Now().Before(deadline) {
		if hello, err := holdProbe(sock); err == nil {
			fmt.Printf("held:    %s\n", host.Name)
			fmt.Printf("pid:     %d\n", hello.Pid)
			fmt.Printf("socket:  %s\n", sock)
			fmt.Printf("idle:    %s\n", holdIdle)
			fmt.Printf("dialMs:  %d\n", hello.DialMs)
			return nil
		}
		select {
		case <-died:
			return fmt.Errorf("hold daemon exited during startup%s\nhint: run `tn status` once to verify auth and pin the host key interactively, then retry", logTail(logPath))
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("hold daemon did not become ready within %s%s", flagTimeout+5*time.Second, logTail(logPath))
}

// logTail returns the last few lines of the daemon log, formatted for
// embedding in an error message.
func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return " — log tail (" + path + "):\n  " + strings.Join(lines, "\n  ")
}
