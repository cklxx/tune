package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// genTestKey writes a private + public key pair to dir and returns the .pub
// path plus the parsed ssh.PublicKey.
func genTestKey(t *testing.T, dir, name string) (pubPath string, pub ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	pubPath = filepath.Join(dir, name+".pub")
	if err := os.WriteFile(pubPath, []byte(pubLine), 0o644); err != nil {
		t.Fatal(err)
	}
	return pubPath, signer.PublicKey()
}

// TestMergeAuthorizedKeysIdempotent is the heart of `tn upload-key`: running
// it repeatedly must not duplicate entries, must dedupe by parsed key (not
// by string), and must preserve foreign entries.
func TestMergeAuthorizedKeysIdempotent(t *testing.T) {
	root := t.TempDir()
	keyDir := t.TempDir()

	pubPath1, pub1 := genTestKey(t, keyDir, "id_a")
	pubPath2, pub2 := genTestKey(t, keyDir, "id_b")

	// Pre-seed authorized_keys with: pub1 (with a different comment), and
	// some unrelated foreign entry.
	mustMkdir(t, filepath.Join(root, ".ssh"), 0o700)
	pre := string(ssh.MarshalAuthorizedKey(pub1))
	pre = strings.TrimSpace(pre) + " alice@old-host\n"
	pre += "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDfakefakefakefakefake foreign@host\n"
	mustWrite(t, filepath.Join(root, ".ssh", "authorized_keys"), []byte(pre), 0o600)

	fc := dialSFTP(t, root)
	keys, err := readPubKeys([]string{pubPath1, pubPath2})
	if err != nil {
		t.Fatal(err)
	}

	// First run: pub1 already there (different comment is fine), pub2 new.
	added, _, err := mergeAuthorizedKeys(fc, keys, false)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("first run: added=%d, want 1", added)
	}

	body := mustRead(t, filepath.Join(root, ".ssh", "authorized_keys"))

	// pub1 line we wrote should still be there (with its comment) — no
	// rewrite of existing entries.
	if !strings.Contains(body, "alice@old-host") {
		t.Errorf("foreign comment on pub1 was overwritten:\n%s", body)
	}
	// foreign entry preserved.
	if !strings.Contains(body, "foreign@host") {
		t.Errorf("foreign entry dropped:\n%s", body)
	}
	// pub2 must now be present.
	if !containsPubKey(body, pub2) {
		t.Errorf("pub2 was not added:\n%s", body)
	}

	// Second run: nothing should change.
	added2, _, err := mergeAuthorizedKeys(fc, keys, false)
	if err != nil {
		t.Fatal(err)
	}
	if added2 != 0 {
		t.Fatalf("second run: added=%d, want 0", added2)
	}
	body2 := mustRead(t, filepath.Join(root, ".ssh", "authorized_keys"))
	if body != body2 {
		t.Errorf("second run mutated the file unexpectedly:\nbefore:\n%s\nafter:\n%s", body, body2)
	}

	// Both keys are findable by fingerprint.
	if !containsPubKey(body2, pub1) || !containsPubKey(body2, pub2) {
		t.Errorf("expected both keys present after merge:\n%s", body2)
	}
}

// TestMergeAuthorizedKeysCreatesFromScratch covers the no-existing-file path.
func TestMergeAuthorizedKeysCreatesFromScratch(t *testing.T) {
	root := t.TempDir()
	keyDir := t.TempDir()
	pubPath, pub := genTestKey(t, keyDir, "id_x")

	fc := dialSFTP(t, root)
	keys, err := readPubKeys([]string{pubPath})
	if err != nil {
		t.Fatal(err)
	}
	added, total, err := mergeAuthorizedKeys(fc, keys, false)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || total != 1 {
		t.Fatalf("added=%d total=%d, want 1/1", added, total)
	}

	st, err := os.Stat(filepath.Join(root, ".ssh", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("authorized_keys perm = %o, want 0600", got)
	}
	if !containsPubKey(mustRead(t, filepath.Join(root, ".ssh", "authorized_keys")), pub) {
		t.Errorf("pub not present after creation")
	}
}

// TestMergeAuthorizedKeysSkipsUnparseableLines ensures a single malformed
// line in the existing file doesn't blow up the whole merge.
func TestMergeAuthorizedKeysSkipsUnparseableLines(t *testing.T) {
	root := t.TempDir()
	keyDir := t.TempDir()
	pubPath, _ := genTestKey(t, keyDir, "id_z")

	mustMkdir(t, filepath.Join(root, ".ssh"), 0o700)
	mustWrite(t, filepath.Join(root, ".ssh", "authorized_keys"), []byte("# comment line\nnot-a-real-key-line\n"), 0o600)

	fc := dialSFTP(t, root)
	keys, err := readPubKeys([]string{pubPath})
	if err != nil {
		t.Fatal(err)
	}
	added, _, err := mergeAuthorizedKeys(fc, keys, false)
	if err != nil {
		t.Fatalf("merge failed on garbage-mixed file: %v", err)
	}
	if added != 1 {
		t.Errorf("added=%d, want 1", added)
	}
}

func TestReadPubKeysMultipleInOneFile(t *testing.T) {
	d := t.TempDir()
	_, pub1 := genTestKey(t, d, "ka")
	_, pub2 := genTestKey(t, d, "kb")

	bundle := filepath.Join(d, "bundle.pub")
	a := string(ssh.MarshalAuthorizedKey(pub1))
	b := string(ssh.MarshalAuthorizedKey(pub2))
	if err := os.WriteFile(bundle, []byte(a+b), 0o644); err != nil {
		t.Fatal(err)
	}

	keys, err := readPubKeys([]string{bundle})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
}
func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func containsPubKey(body string, key ssh.PublicKey) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		if ssh.FingerprintSHA256(parsed) == ssh.FingerprintSHA256(key) {
			return true
		}
	}
	return false
}
