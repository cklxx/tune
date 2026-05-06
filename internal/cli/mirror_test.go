package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestMirrorOnceIdempotent verifies a fresh mirror copies the tree and
// running it again does nothing.
func TestMirrorOnceIdempotent(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a.txt":          "alpha",
		"sub/b.go":       "package sub",
		"sub/deep/c.md":  "# title",
		"node_modules/x": "junk",
	}
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fc := dialSFTP(t, root)
	dst := t.TempDir()

	var buf bytes.Buffer
	stats, err := mirrorOnceRun(fc, ".", dst, nil, nil, false, 4, &buf)
	if err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	if stats.Added != len(files) {
		t.Fatalf("added=%d, want %d", stats.Added, len(files))
	}
	for p, body := range files {
		got, err := os.ReadFile(filepath.Join(dst, p))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if string(got) != body {
			t.Errorf("%s = %q, want %q", p, got, body)
		}
	}

	// Re-run: nothing should change.
	buf.Reset()
	stats2, err := mirrorOnceRun(fc, ".", dst, nil, nil, false, 4, &buf)
	if err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	if stats2.Added != 0 || stats2.Modified != 0 {
		t.Errorf("second run not idempotent: %+v\noutput:\n%s", stats2, buf.String())
	}
	if stats2.Unchanged != len(files) {
		t.Errorf("unchanged=%d want %d", stats2.Unchanged, len(files))
	}
}

// TestMirrorDetectsModification ensures touching a remote file (changing size
// OR mtime) causes a re-download.
func TestMirrorDetectsModification(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "a.txt")
	if err := os.WriteFile(src, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	fc := dialSFTP(t, root)
	dst := t.TempDir()
	var buf bytes.Buffer
	if _, err := mirrorOnceRun(fc, ".", dst, nil, nil, false, 1, &buf); err != nil {
		t.Fatal(err)
	}

	// Modify content + mtime.
	if err := os.WriteFile(src, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(src, future, future)

	buf.Reset()
	stats, err := mirrorOnceRun(fc, ".", dst, nil, nil, false, 1, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Modified != 1 {
		t.Errorf("expected 1 modified, got %+v\nout: %s", stats, buf.String())
	}
	got, _ := os.ReadFile(filepath.Join(dst, "a.txt"))
	if string(got) != "v2-longer" {
		t.Errorf("after mod: %q", got)
	}
}

// TestMirrorDelete verifies --delete removes locals that no longer exist.
func TestMirrorDelete(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"keep.txt", "drop/me.txt"} {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte("x"), 0o644)
	}

	fc := dialSFTP(t, root)
	dst := t.TempDir()
	var buf bytes.Buffer
	if _, err := mirrorOnceRun(fc, ".", dst, nil, nil, false, 1, &buf); err != nil {
		t.Fatal(err)
	}

	// Remove "drop/me.txt" remotely.
	if err := os.RemoveAll(filepath.Join(root, "drop")); err != nil {
		t.Fatal(err)
	}

	// Without --delete: stale file persists.
	buf.Reset()
	if _, err := mirrorOnceRun(fc, ".", dst, nil, nil, false, 1, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "drop", "me.txt")); err != nil {
		t.Errorf("without --delete the file should still exist: %v", err)
	}

	// With --delete.
	buf.Reset()
	stats, err := mirrorOnceRun(fc, ".", dst, nil, nil, true, 1, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted < 1 {
		t.Errorf("expected at least one deletion, got %+v", stats)
	}
	if _, err := os.Stat(filepath.Join(dst, "drop", "me.txt")); !os.IsNotExist(err) {
		t.Errorf("file should be gone, got err=%v", err)
	}
}

// TestMirrorIncludeExclude verifies the filter logic.
func TestMirrorIncludeExclude(t *testing.T) {
	root := t.TempDir()
	files := []string{"a.go", "b.go", "c.txt", "vendor/x.go"}
	for _, p := range files {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte("x"), 0o644)
	}

	fc := dialSFTP(t, root)
	dst := t.TempDir()
	var buf bytes.Buffer
	stats, err := mirrorOnceRun(fc, ".", dst, []string{"*.go"}, []string{"vendor/*"}, false, 1, &buf)
	if err != nil {
		t.Fatal(err)
	}

	// Walk dst and verify what's there.
	var got []string
	_ = filepath.Walk(dst, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dst, p)
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(got)

	want := []string{"a.go", "b.go"}
	if len(got) != len(want) {
		t.Fatalf("downloaded %v, want %v (stats=%+v out:\n%s)", got, want, stats, buf.String())
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("at %d: %s vs %s", i, got[i], want[i])
		}
	}
}

func TestMatchFilter(t *testing.T) {
	cases := []struct {
		rel       string
		isDir     bool
		incs, exs []string
		want      bool
	}{
		{"a.go", false, nil, nil, true},
		{"a.go", false, []string{"*.go"}, nil, true},
		{"a.txt", false, []string{"*.go"}, nil, false},
		{"vendor/a.go", false, []string{"*.go"}, []string{"vendor/*"}, false},
		{"sub", true, []string{"*.go"}, nil, true}, // dirs always pass include
		{"node_modules", true, nil, []string{"node_modules"}, false},
	}
	for _, c := range cases {
		got := matchFilter(c.rel, c.incs, c.exs, c.isDir)
		if got != c.want {
			t.Errorf("matchFilter(%q, dir=%v, inc=%v, exc=%v) = %v, want %v",
				c.rel, c.isDir, c.incs, c.exs, got, c.want)
		}
	}
}
