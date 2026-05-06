//go:build windows

package cli

import (
	"context"

	"golang.org/x/crypto/ssh"
)

// Windows console resize is delivered via console events, not POSIX signals,
// and there is no portable equivalent of SIGWINCH. We start the PTY at the
// initial terminal size from term.GetSize and don't track subsequent resizes.
func watchWindowSize(_ context.Context, _ int, _ *ssh.Session) {}
