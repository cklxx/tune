package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cklxx/tune/internal/sshx"
	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
)

var (
	mirrorOnce     bool
	mirrorInterval time.Duration
	mirrorDelete   bool
	mirrorIncludes []string
	mirrorExcludes []string
	mirrorWorkers  int
)

var mirrorCmd = &cobra.Command{
	Use:   "mirror <remote-path> <local-path>",
	Short: "Maintain a local mirror of a remote directory (poll-based)",
	Long: `Walks the remote tree and downloads any file whose (size, mtime)
differs from the local copy. Use --once for a single sync (good for CI or
agent flows), or omit it to keep watching at --interval.

This is the "easy code reading" path: with a fresh mirror, run grep, your
LSP, your IDE locally — no SSH round trip per file.

Note: this is a one-way pull from remote → local. For bidirectional sync
with conflict resolution, use Mutagen.

Filter the tree with --include/--exclude (repeatable, glob syntax matched
against POSIX-style relative paths). With --delete, files removed remotely
are also removed locally.

Examples:
  tn mirror /srv/repo ./mirror --once
  tn mirror /var/log ./logs --include '*.log' --interval 5s
  tn mirror /workspace ~/work --delete`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		remote, local := args[0], args[1]
		if err := os.MkdirAll(local, 0o755); err != nil {
			return err
		}

		c, _, err := connect()
		if err != nil {
			return err
		}
		defer c.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() { <-sigCh; cancel() }()

		runOnce := func() error {
			fc, err := newSFTP(c)
			if err != nil {
				return err
			}
			defer fc.Close()
			stats, err := mirrorOnceRun(fc, remote, local, mirrorIncludes, mirrorExcludes, mirrorDelete, mirrorWorkers, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if !flagJSON {
				fmt.Fprintf(cmd.ErrOrStderr(), "mirror: +%d ~%d -%d (= %d unchanged) in %dms\n",
					stats.Added, stats.Modified, stats.Deleted, stats.Unchanged, stats.ElapsedMs)
			}
			return nil
		}

		if mirrorOnce {
			return runOnce()
		}
		// Watch loop.
		fmt.Fprintf(cmd.ErrOrStderr(), "tn mirror: watching every %s — Ctrl-C to stop\n", mirrorInterval)
		for {
			if err := runOnce(); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "mirror: %v\n", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(mirrorInterval):
			}
		}
	},
}

func init() {
	mirrorCmd.Flags().BoolVar(&mirrorOnce, "once", false, "sync once and exit")
	mirrorCmd.Flags().DurationVar(&mirrorInterval, "interval", 3*time.Second, "poll interval in watch mode")
	mirrorCmd.Flags().BoolVar(&mirrorDelete, "delete", false, "remove local files that no longer exist remotely")
	mirrorCmd.Flags().StringSliceVar(&mirrorIncludes, "include", nil, "glob to include (repeatable; default: include everything)")
	mirrorCmd.Flags().StringSliceVar(&mirrorExcludes, "exclude", nil, "glob to exclude (repeatable)")
	mirrorCmd.Flags().IntVar(&mirrorWorkers, "workers", 4, "concurrent file downloads")
}

type mirrorStats struct {
	Added     int   `json:"added"`
	Modified  int   `json:"modified"`
	Deleted   int   `json:"deleted"`
	Unchanged int   `json:"unchanged"`
	ElapsedMs int64 `json:"elapsedMs"`
}

// mirrorOnceRun walks remote, downloads anything whose (size, mtime) differs,
// and (if del=true) removes locals not seen remotely. Concurrency is bounded
// by workers. Returns stats.
func mirrorOnceRun(fc *sftp.Client, remote, local string, includes, excludes []string, del bool, workers int, jsonOut io.Writer) (mirrorStats, error) {
	start := time.Now()
	var stats mirrorStats

	if workers < 1 {
		workers = 1
	}

	// Collect remote tree first (cheap stat-only walk).
	type remoteFile struct {
		rel   string
		size  int64
		mtime time.Time
		isDir bool
	}
	var remoteFiles []remoteFile
	walker := fc.Walk(remote)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return stats, err
		}
		p := walker.Path()
		info := walker.Stat()
		rel := strings.TrimPrefix(p, remote)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		if !matchFilter(rel, includes, excludes, info.IsDir()) {
			if info.IsDir() {
				walker.SkipDir()
			}
			continue
		}
		remoteFiles = append(remoteFiles, remoteFile{rel: rel, size: info.Size(), mtime: info.ModTime(), isDir: info.IsDir()})
	}

	// Index local tree (only files we'd compete with).
	localFiles := map[string]os.FileInfo{}
	_ = filepath.WalkDir(local, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(local, p)
		if err != nil || rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		localFiles[filepath.ToSlash(rel)] = info
		return nil
	})

	// Plan: which files need download / mkdir.
	type job struct {
		rel  string
		rf   remoteFile
		kind string // "add" | "mod"
	}
	var jobs []job
	seen := map[string]bool{}
	for _, rf := range remoteFiles {
		seen[rf.rel] = true
		if rf.isDir {
			if err := os.MkdirAll(filepath.Join(local, rf.rel), 0o755); err != nil {
				return stats, err
			}
			continue
		}
		if li, ok := localFiles[rf.rel]; ok {
			if li.Size() == rf.size && li.ModTime().Truncate(time.Second).Equal(rf.mtime.Truncate(time.Second)) {
				stats.Unchanged++
				continue
			}
			jobs = append(jobs, job{rel: rf.rel, rf: rf, kind: "mod"})
		} else {
			jobs = append(jobs, job{rel: rf.rel, rf: rf, kind: "add"})
		}
	}

	// Run downloads concurrently. emit is serialized — workers and the
	// post-loop delete pass all funnel through it.
	type event struct {
		Kind string `json:"kind"` // add|mod|del
		Path string `json:"path"`
		Err  string `json:"error,omitempty"`
	}
	var emitMu sync.Mutex
	emit := func(e event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		if flagJSON {
			_ = json.NewEncoder(jsonOut).Encode(e)
			return
		}
		sym := "+"
		if e.Kind == "mod" {
			sym = "M"
		} else if e.Kind == "del" {
			sym = "-"
		}
		if e.Err != "" {
			fmt.Fprintf(jsonOut, "%s %s   ERROR: %s\n", sym, e.Path, e.Err)
		} else {
			fmt.Fprintf(jsonOut, "%s %s\n", sym, e.Path)
		}
	}

	jobsCh := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobsCh {
				dst := filepath.Join(local, filepath.FromSlash(j.rel))
				src := path.Join(remote, j.rel)
				if err := pullFile(fc, src, dst, 0o644); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					emit(event{Kind: j.kind, Path: j.rel, Err: err.Error()})
					continue
				}
				// Match remote mtime so the next poll sees us as up-to-date.
				_ = os.Chtimes(dst, j.rf.mtime, j.rf.mtime)
				mu.Lock()
				if j.kind == "add" {
					stats.Added++
				} else {
					stats.Modified++
				}
				mu.Unlock()
				emit(event{Kind: j.kind, Path: j.rel})
			}
		}()
	}
	for _, j := range jobs {
		jobsCh <- j
	}
	close(jobsCh)
	wg.Wait()

	if del {
		// Sort longest-first so we delete files before their parent dirs.
		var rels []string
		for r := range localFiles {
			if !seen[r] {
				rels = append(rels, r)
			}
		}
		sort.Slice(rels, func(i, j int) bool { return len(rels[i]) > len(rels[j]) })
		for _, r := range rels {
			full := filepath.Join(local, filepath.FromSlash(r))
			if err := os.RemoveAll(full); err != nil {
				emit(event{Kind: "del", Path: r, Err: err.Error()})
				continue
			}
			stats.Deleted++
			emit(event{Kind: "del", Path: r})
		}
	}

	stats.ElapsedMs = time.Since(start).Milliseconds()
	if firstErr != nil {
		return stats, fmt.Errorf("some files failed to download: %w", firstErr)
	}
	return stats, nil
}

// matchFilter returns true if rel passes the include/exclude filters. Empty
// includes means "include everything"; excludes always wins. Patterns are
// path.Match globs against the POSIX relative path.
func matchFilter(rel string, includes, excludes []string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	for _, e := range excludes {
		if ok, _ := path.Match(e, rel); ok {
			return false
		}
		if ok, _ := path.Match(e, path.Base(rel)); ok {
			return false
		}
	}
	if len(includes) == 0 || isDir {
		return true
	}
	for _, in := range includes {
		if ok, _ := path.Match(in, rel); ok {
			return true
		}
		if ok, _ := path.Match(in, path.Base(rel)); ok {
			return true
		}
	}
	return false
}

// guard against unused-import drift on platforms that strip dead code paths.
var (
	_ = errors.New
	_ = sshx.PolicyTOFU
)
