//go:build windows

package cli

import (
	"errors"

	"github.com/cklxx/tune/internal/config"
)

// Windows has no Setsid-style detach and tn's hold path is Unix-socket
// based, so hold is disabled entirely: holdSupported=false makes every
// command direct-dial, exactly as before hold existed.
const holdSupported = false

func withHoldLock(_ *config.Host, f func() error) error { return f() }

func ensureHold(_ *config.Host, _ bool) error {
	return errors.New("tn hold is not supported on Windows — commands dial directly")
}

func spawnHold(_ *config.Host, _ bool) error {
	return errors.New("tn hold is not supported on Windows — commands dial directly")
}
