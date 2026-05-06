package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

var shellCmd = &cobra.Command{
	Use:   "shell",
	Short: "Open an interactive PTY shell on the remote",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()

		sess, err := c.SSH().NewSession()
		if err != nil {
			return err
		}
		defer sess.Close()

		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return errors.New("stdin is not a terminal; use `tn exec` for non-interactive runs")
		}
		old, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, old)

		w, h, err := term.GetSize(fd)
		if err != nil {
			w, h = 80, 24
		}
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		termType := os.Getenv("TERM")
		if termType == "" {
			termType = "xterm-256color"
		}
		if err := sess.RequestPty(termType, h, w, modes); err != nil {
			return err
		}

		// Forward window resizes — Unix uses SIGWINCH; Windows is a stub
		// for now (see shell_windows.go).
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		watchWindowSize(ctx, fd, sess)

		sess.Stdin = os.Stdin
		sess.Stdout = os.Stdout
		sess.Stderr = os.Stderr

		if err := sess.Shell(); err != nil {
			return err
		}
		err = sess.Wait()
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitStatus())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return nil
	},
}
