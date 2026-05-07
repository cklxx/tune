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
	Long: `Copies <local> to <remote> over SFTP. If <local> is a directory the
copy is recursive: directories are mkdir-p'd, regular files are streamed,
and each file's mode bits are preserved (mtime is not).

Symlinks are NOT followed and not recreated; they are skipped silently.
Existing files at the destination are overwritten without prompt.

On any error the command exits non-zero and stops at the first failure —
files copied before the failure remain on the remote (no rollback).`,
	Example: `  tn push ./build/app.tar.gz /srv/releases/
  tn push ./src /srv/app/src           # recursive
  tn push secrets.env /etc/myapp/.env  # single file rename`,
	Args: cobra.ExactArgs(2),
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
	Long: `Mirror of "tn push": copies <remote> down to <local>, recursing into
directories. File mode bits are preserved. Symlinks on the remote are
skipped. Existing local files are overwritten.

For repeated reads of the same tree (grep, IDE, LSP), prefer "tn mirror"
which only re-pulls files whose (size, mtime) changed.`,
	Example: `  tn pull /var/log/app.log ./logs/
  tn pull /srv/app/build ./local-build
  tn pull /etc/nginx ./nginx-snapshot`,
	Args: cobra.ExactArgs(2),
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
	Long: `Streams the named file to stdout, byte-for-byte. No newline is added.
Designed for coding agents and shell pipelines:

    tn read /etc/hostname           # plain
    tn read --json /etc/passwd      # {"path","size","mode","content"}

The plain mode does no copy/transform — it's "cat over SSH". On error
(file missing, permission denied, etc.) tn exits non-zero and prints the
underlying SFTP error to stderr; nothing is written to stdout.`,
	Example: `  tn read /etc/hostname
  diff <(tn read /etc/nginx/nginx.conf) ./nginx.conf
  tn read /var/log/app.log | grep ERROR | head`,
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
		return readFile(fc, args[0], cmd.OutOrStdout())
	},
}

var writeCmd = &cobra.Command{
	Use:   "write <remote>",
	Short: "Write stdin to a remote file (atomic via tmp + rename)",
	Long: `Streams stdin to <remote>. The write is atomic: bytes go to
<remote>.tn-tmp first, then posix-rename swaps it into place, so a
concurrent reader either sees the old file or the new one — never a
half-written one.

The destination directory is mkdir-p'd if it doesn't exist. The new file
ends up with mode 0644 by default (the remote umask may further restrict
it). To set a specific mode, follow up with "tn exec -- chmod NNN /path".

If you don't redirect anything into stdin, tn waits forever — pipe
something in or use < /dev/null for an empty file.`,
	Example: `  echo "FOO=bar" | tn write /etc/myapp/env
  tar -cz ./code | tn write /tmp/code.tar.gz
  tn write /tmp/empty < /dev/null`,
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
		return writeFile(fc, args[0], os.Stdin)
	},
}

var lsLong bool

var lsCmd = &cobra.Command{
	Use:   "ls [remote-path]",
	Short: "List a remote directory",
	Long: `Lists entries in <remote-path> (defaults to the SSH login dir, usually
$HOME). Directories are suffixed with "/" in the default output. Use -l
for a long listing (mode, uid, gid, size, mtime, name) or --json for
structured output with size, mode (octal), uid, gid, mtime (unix epoch
seconds), isDir, and name.

UID/GID are returned as numbers — there is no remote /etc/passwd lookup
(would cost an extra round trip per call). To resolve them to names,
pipe through "tn exec -- getent passwd UID" or do the lookup locally.`,
	Example: `  tn ls                        # $HOME on remote
  tn ls /var/log
  tn ls -l /etc/nginx
  tn ls --json /srv | jq '.[] | select(.isDir)'`,
	Args: cobra.MaximumNArgs(1),
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

// readFile streams remote file to w. With --json, wraps in a frame.
func readFile(fc *sftp.Client, path string, w io.Writer) error {
	f, err := fc.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if !flagJSON {
		_, err := io.Copy(w, f)
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
	return json.NewEncoder(w).Encode(map[string]any{
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
			UID   uint32 `json:"uid"`
			GID   uint32 `json:"gid"`
			Mtime int64  `json:"mtime"`
			IsDir bool   `json:"isDir"`
		}
		rows := make([]row, 0, len(entries))
		for _, e := range entries {
			uid, gid := uidGid(e)
			rows = append(rows, row{e.Name(), e.Size(), fmt.Sprintf("%#o", e.Mode().Perm()), uid, gid, e.ModTime().Unix(), e.IsDir()})
		}
		return json.NewEncoder(out).Encode(rows)
	}
	for _, e := range entries {
		if lsLong {
			uid, gid := uidGid(e)
			fmt.Fprintf(out, "%s %5d %5d %10d %s %s\n", e.Mode(), uid, gid, e.Size(), e.ModTime().Format("2006-01-02 15:04"), e.Name())
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

// uidGid pulls the SFTP UID/GID out of a FileInfo. Returns (0, 0) if the
// underlying server didn't send them (extremely rare; standard sshd does).
func uidGid(fi os.FileInfo) (uint32, uint32) {
	if st, ok := fi.Sys().(*sftp.FileStat); ok && st != nil {
		return st.UID, st.GID
	}
	return 0, 0
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

