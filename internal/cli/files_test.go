package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshtest"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/pkg/sftp"
)

// dialSFTP brings up an SFTP-capable test server rooted at root, dials it,
// and returns a connected sftp.Client. All cleanup is registered on t.
func dialSFTP(t *testing.T, root string) *sftp.Client {
	t.Helper()
	kp := sshtest.GenKey(t)
	srv := sshtest.Start(t, sshtest.Options{
		AllowedKey: kp.PublicKey,
		AllowSFTP:  true,
		SFTPRoot:   root,
	})

	host := &config.Host{
		Target:     config.Hop{Addr: srv.Addr, User: "alice", IdentityFile: kp.Path},
		KnownHosts: filepath.Join(t.TempDir(), "kh"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sshClient, err := sshx.Dial(ctx, host, sshx.PolicyInsecure)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { sshClient.Close() })

	fc, err := sftp.NewClient(sshClient.SSH(),
		sftp.UseConcurrentReads(true),
		sftp.UseConcurrentWrites(true),
	)
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}
	t.Cleanup(func() { fc.Close() })
	return fc
}

func TestPushPullRoundTrip(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()

	// Build a small local tree.
	for path, body := range map[string]string{
		"a.txt":          "hello",
		"sub/b.txt":      "world",
		"sub/deep/c.txt": "tune",
	} {
		full := filepath.Join(srcDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fc := dialSFTP(t, root)

	if err := push(fc, srcDir, "uploaded"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Verify the tree landed on the SFTP-rooted dir.
	got := map[string]string{}
	_ = filepath.Walk(filepath.Join(root, "uploaded"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(root, "uploaded"), p)
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	want := map[string]string{
		"a.txt":          "hello",
		"sub/b.txt":      "world",
		"sub/deep/c.txt": "tune",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("after push: %s = %q, want %q", k, got[k], v)
		}
	}

	// Now pull it back into a third directory and diff.
	pullDir := t.TempDir()
	if err := pull(fc, "uploaded", pullDir); err != nil {
		t.Fatalf("pull: %v", err)
	}
	for k, v := range want {
		body, err := os.ReadFile(filepath.Join(pullDir, k))
		if err != nil {
			t.Fatalf("pulled %s: %v", k, err)
		}
		if string(body) != v {
			t.Errorf("pulled %s = %q, want %q", k, body, v)
		}
	}
}

func TestWriteFileAtomicity(t *testing.T) {
	root := t.TempDir()
	fc := dialSFTP(t, root)

	target := "atomic.txt"
	if err := writeFile(fc, target, strings.NewReader("v1")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, target))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v1" {
		t.Errorf("first write: %q", body)
	}

	// Overwrite — must replace cleanly.
	if err := writeFile(fc, target, strings.NewReader("v2-longer-payload")); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(root, target))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2-longer-payload" {
		t.Errorf("overwrite: %q", body)
	}

	// No tmp file should remain.
	if _, err := os.Stat(filepath.Join(root, target+".tn-tmp")); !os.IsNotExist(err) {
		t.Errorf("tmp file leaked: %v", err)
	}
}

func TestReadFilePlainAndJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "greet.txt"), []byte("hi tn"), 0o600); err != nil {
		t.Fatal(err)
	}
	fc := dialSFTP(t, root)

	var buf bytes.Buffer

	flagJSON = false
	if err := readFile(fc, "greet.txt", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hi tn" {
		t.Errorf("plain: %q", buf.String())
	}

	buf.Reset()
	flagJSON = true
	t.Cleanup(func() { flagJSON = false })
	if err := readFile(fc, "greet.txt", &buf); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		Mode    string `json:"mode"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, buf.String())
	}
	if got.Path != "greet.txt" || got.Content != "hi tn" || got.Size != 5 {
		t.Errorf("json fields: %+v", got)
	}
}

func TestList(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha.txt", "beta.txt"} {
		_ = os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	fc := dialSFTP(t, root)

	var buf bytes.Buffer
	flagJSON = false
	if err := list(fc, ".", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"alpha.txt", "beta.txt", "sub/"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in plain output, got:\n%s", want, out)
		}
	}

	buf.Reset()
	flagJSON = true
	t.Cleanup(func() { flagJSON = false })
	if err := list(fc, ".", &buf); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"isDir"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, buf.String())
	}
	if len(rows) != 3 {
		t.Errorf("expected 3 entries, got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Name == "sub" && !r.IsDir {
			t.Error("sub should be IsDir=true")
		}
	}
}

// Compile-time pin so future refactors don't accidentally break these helpers.
var (
	_ = time.Second
)
