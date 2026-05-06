//go:build !windows

package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// watchWindowSize subscribes to SIGWINCH and forwards each terminal resize to
// the remote session via SSH "window-change". Returns when ctx is cancelled.
func watchWindowSize(ctx context.Context, fd int, sess *ssh.Session) {
	winCh := make(chan os.Signal, 1)
	signal.Notify(winCh, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(winCh)
		for {
			select {
			case <-ctx.Done():
				return
			case <-winCh:
				if w, h, err := term.GetSize(fd); err == nil {
					_ = sess.WindowChange(h, w)
				}
			}
		}
	}()
}
