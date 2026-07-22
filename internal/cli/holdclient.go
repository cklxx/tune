package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/cklxx/tune/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// tryHeld routes req through a live hold daemon for the resolved host.
// Eligible non-interactive calls quietly start one when absent. ok=false
// means the caller must fall back to a direct dial; host is returned so that
// fallback need not reload and resolve config. Once the request frame is on
// the wire the op is committed: errors after that are returned as errors,
// never silently re-run (the op may have had side effects on the remote).
func tryHeld(req holdRequest, stdin io.Reader, stdout, stderr io.Writer) (code int, ok bool, host *config.Host, err error) {
	if !holdSupported {
		return 0, false, nil, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return 0, false, nil, nil
	}
	host, err = cfg.Resolve(flagHost)
	if err != nil {
		return 0, false, nil, nil
	}
	conn, hello, err := holdAttach(holdSocketPath(host.Name))
	if err != nil && holdAutoStartEligible(req, stdin) {
		if ensureHold(host, true) == nil {
			conn, hello, err = holdAttach(holdSocketPath(host.Name))
		}
	}
	if err != nil {
		return 0, false, host, nil
	}
	defer conn.Close()
	if hello.Config != holdConfigID(host, currentPolicy()) {
		fmt.Fprintf(os.Stderr, "tn: held connection for %q uses stale connection settings — dialing direct (tn hold --stop && tn hold to refresh)\n", host.Name)
		return 0, false, host, nil
	}
	code, err = holdRoundTrip(newFrameConn(conn), req, stdin, stdout, stderr)
	return code, true, host, err
}

func holdAutoStartEligible(req holdRequest, stdin io.Reader) bool {
	switch req.Op {
	case "read", "ls", "push", "pull":
		return true
	case "exec", "write":
		if f, ok := stdin.(*os.File); ok {
			return !term.IsTerminal(int(f.Fd()))
		}
		return stdin != nil
	default:
		return false
	}
}

// holdRoundTrip sends one request and pumps frames until EXIT. For exec it
// also forwards local SIGINT/SIGTERM to the remote process, mirroring the
// direct path.
func holdRoundTrip(fc *frameConn, req holdRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := fc.writeJSON(frameReq, req); err != nil {
		return 0, fmt.Errorf("hold daemon: send request: %w", err)
	}

	if req.Op == "exec" {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		go func() {
			for s := range sigCh {
				switch s {
				case syscall.SIGINT:
					_ = fc.writeFrame(frameSignal, []byte(ssh.SIGINT))
				case syscall.SIGTERM:
					_ = fc.writeFrame(frameSignal, []byte(ssh.SIGTERM))
				}
			}
		}()
	}

	if stdin != nil {
		go func() {
			buf := make([]byte, holdChunkSize)
			for {
				n, err := stdin.Read(buf)
				if n > 0 {
					if werr := fc.writeFrame(frameStdin, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					_ = fc.writeFrame(frameStdinEOF, nil)
					return
				}
			}
		}()
	} else {
		_ = fc.writeFrame(frameStdinEOF, nil)
	}

	for {
		t, payload, err := fc.readFrame()
		if err != nil {
			return 0, fmt.Errorf("held connection lost mid-operation: %w", err)
		}
		switch t {
		case frameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return 0, err
			}
		case frameStderr:
			if _, err := stderr.Write(payload); err != nil {
				return 0, err
			}
		case frameExit:
			var x holdExit
			if err := json.Unmarshal(payload, &x); err != nil {
				return 0, err
			}
			if x.Err != "" {
				return x.Code, errors.New(x.Err)
			}
			return x.Code, nil
		}
	}
}

// heldResult adapts a completed held op to RunE semantics: transport/SFTP
// errors surface as errors, a nonzero remote exit code becomes our exit
// code — same contract as the direct path.
func heldResult(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// heldInfo queries a running daemon's state (op=info). ok=false when no
// usable daemon is up.
func heldInfo() (*holdInfo, bool) {
	var buf bytes.Buffer
	code, ok, _, err := tryHeld(holdRequest{Op: "info"}, nil, &buf, io.Discard)
	if !ok || err != nil || code != 0 {
		return nil, false
	}
	info := &holdInfo{}
	if err := json.Unmarshal(buf.Bytes(), info); err != nil {
		return nil, false
	}
	return info, true
}
