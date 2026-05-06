package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
)

var pushCmd = &cobra.Command{
	Use:   "push <local> <remote>",
	Short: "Upload a file or directory to the remote",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()
		fc, err := newSFTP(c)
		if err != nil {
			return err
		}
		defer fc.Close()
		return push(fc, args[0], args[1])
	},
}

var pullCmd = &cobra.Command{
	Use:   "pull <remote> <local>",
	Short: "Download a file or directory from the remote",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()
		fc, err := newSFTP(c)
		if err != nil {
			return err
		}
		defer fc.Close()
		return pull(fc, args[0], args[1])
	},
}

var readCmd = &cobra.Command{
	Use:   "read <remote>",
	Short: "Print the contents of a remote file to stdout",
	Long: `Streams the named file to stdout. Designed for coding agents that
want to fetch a remote source file inline; use --json for a structured wrapper.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()
		fc, err := newSFTP(c)
		if err != nil {
			return err
		}
		defer fc.Close()
		return readFile(fc, args[0])
	},
}

var writeCmd = &cobra.Command{
	Use:   "write <remote>",
	Short: "Write stdin to a remote file (atomic via tmp + rename)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()
		fc, err := newSFTP(c)
		if err != nil {
			return err
		}
		defer fc.Close()
		return writeFile(fc, args[0], os.Stdin)
	},
}

var lsLong bool

var lsCmd = &cobra.Command{
	Use:   "ls [remote-path]",
	Short: "List a remote directory",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := "."
		if len(args) == 1 {
			path = args[0]
		}
		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()
		fc, err := newSFTP(c)
		if err != nil {
			return err
		}
		defer fc.Close()
		return list(fc, path, cmd.OutOrStdout())
	},
}

func init() {
	lsCmd.Flags().BoolVarP(&lsLong, "long", "l", false, "long format with size and mtime")
}

// readFile streams remote file to stdout. With --json, wraps in a frame.
func readFile(fc *sftp.Client, path string) error {
	f, err := fc.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if !flagJSON {
		_, err := io.Copy(os.Stdout, f)
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"path":    path,
		"size":    st.Size(),
		"mode":    fmt.Sprintf("%#o", st.Mode().Perm()),
		"content": string(data),
	})
}

// writeFile writes src to a temp path on the remote then renames atomically.
func writeFile(fc *sftp.Client, path string, src io.Reader) error {
	dir := filepath.ToSlash(filepath.Dir(path))
	if dir != "" && dir != "." {
		_ = fc.MkdirAll(dir)
	}
	tmp := path + ".tn-tmp"
	f, err := fc.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		_ = fc.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = fc.Remove(tmp)
		return err
	}
	// PosixRename overrides any existing file atomically; falls back to
	// non-atomic on servers without the extension.
	if err := fc.PosixRename(tmp, path); err != nil {
		// Best-effort fallback.
		_ = fc.Remove(path)
		if rerr := fc.Rename(tmp, path); rerr != nil {
			_ = fc.Remove(tmp)
			return rerr
		}
	}
	return nil
}

// list emits a directory listing. JSON mode is structured.
func list(fc *sftp.Client, path string, out io.Writer) error {
	entries, err := fc.ReadDir(path)
	if err != nil {
		return err
	}
	if flagJSON {
		type row struct {
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			Mode  string `json:"mode"`
			Mtime int64  `json:"mtime"`
			IsDir bool   `json:"isDir"`
		}
		out := make([]row, 0, len(entries))
		for _, e := range entries {
			out = append(out, row{e.Name(), e.Size(), fmt.Sprintf("%#o", e.Mode().Perm()), e.ModTime().Unix(), e.IsDir()})
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	for _, e := range entries {
		if lsLong {
			fmt.Fprintf(out, "%s %10d %s %s\n", e.Mode(), e.Size(), e.ModTime().Format("2006-01-02 15:04"), e.Name())
		} else {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			fmt.Fprintln(out, name)
		}
	}
	return nil
}

// push copies local (file or dir) to remote.
func push(fc *sftp.Client, local, remote string) error {
	st, err := os.Stat(local)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return pushFile(fc, local, remote, st)
	}
	return filepath.WalkDir(local, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(local, path)
		if err != nil {
			return err
		}
		dst := filepath.ToSlash(filepath.Join(remote, rel))
		if d.IsDir() {
			return fc.MkdirAll(dst)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return pushFile(fc, path, dst, info)
	})
}

func pushFile(fc *sftp.Client, local, remote string, info os.FileInfo) error {
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()
	dir := filepath.ToSlash(filepath.Dir(remote))
	if dir != "" && dir != "." {
		_ = fc.MkdirAll(dir)
	}
	dst, err := fc.OpenFile(remote, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	_ = fc.Chmod(remote, info.Mode().Perm())
	return nil
}

// pull is the mirror of push.
func pull(fc *sftp.Client, remote, local string) error {
	st, err := fc.Stat(remote)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return pullFile(fc, remote, local, st.Mode().Perm())
	}
	walker := fc.Walk(remote)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return err
		}
		path := walker.Path()
		info := walker.Stat()
		rel := strings.TrimPrefix(path, remote)
		rel = strings.TrimPrefix(rel, "/")
		dst := filepath.Join(local, rel)
		if info.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := pullFile(fc, path, dst, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func pullFile(fc *sftp.Client, remote, local string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	src, err := fc.Open(remote)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

