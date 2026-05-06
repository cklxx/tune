package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshx"
	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

var (
	uploadKeyJump  bool
	uploadKeyForce bool
)

var uploadKeyCmd = &cobra.Command{
	Use:   "upload-key [pubkey-path...]",
	Short: "Append local public keys to the remote ~/.ssh/authorized_keys (idempotent)",
	Long: `Pushes one or more local public keys to ~/.ssh/authorized_keys on the
remote, deduplicating by parsed key (so re-running won't add the same key
twice, and key comments don't matter).

With no arguments, the default candidate paths are tried in order:

    ~/.ssh/id_ed25519.pub
    ~/.ssh/id_ecdsa.pub
    ~/.ssh/id_rsa.pub

Existing files are preserved — entries are merged, never replaced. The
final write is atomic (tmp + posix-rename) and the file is chmodded to
0600 with the parent ~/.ssh chmodded to 0700.

This is the natural follow-up after the first password-authenticated
connect: run "tn upload-key" once, switch your config to identityFile, and
never type the password again.

With --jump, the keys are also uploaded to the configured jump host (a
separate connection).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		paths, err := resolvePubKeyPaths(args)
		if err != nil {
			return err
		}
		keys, err := readPubKeys(paths)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return errors.New("no public keys found; pass paths explicitly or generate one with `ssh-keygen -t ed25519`")
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}
		host, err := cfg.Resolve(flagHost)
		if err != nil {
			return err
		}
		policy := sshx.PolicyTOFU
		if flagInsecure {
			policy = sshx.PolicyInsecure
		}

		if uploadKeyJump {
			if host.Jump == nil {
				return errors.New("--jump given but no jump host is configured")
			}
			fmt.Fprintln(cmd.OutOrStderr(), "uploading to jump host:")
			if err := uploadKeyToOne(cmd, &config.Host{Target: *host.Jump, KnownHosts: host.KnownHosts}, keys, policy); err != nil {
				return fmt.Errorf("jump: %w", err)
			}
		}

		fmt.Fprintln(cmd.OutOrStderr(), "uploading to target:")
		return uploadKeyToOne(cmd, host, keys, policy)
	},
}

func init() {
	uploadKeyCmd.Flags().BoolVar(&uploadKeyJump, "jump", false, "also upload to the configured jump host")
	uploadKeyCmd.Flags().BoolVar(&uploadKeyForce, "force", false, "rewrite authorized_keys even if no changes were needed")
}

// resolvePubKeyPaths returns the user-supplied paths or the default candidate
// list (filtered to those that actually exist).
func resolvePubKeyPaths(args []string) ([]string, error) {
	if len(args) > 0 {
		expanded := make([]string, len(args))
		for i, a := range args {
			expanded[i] = config.ExpandPath(a)
		}
		return expanded, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	candidates := []string{
		filepath.Join(home, ".ssh", "id_ed25519.pub"),
		filepath.Join(home, ".ssh", "id_ecdsa.pub"),
		filepath.Join(home, ".ssh", "id_rsa.pub"),
	}
	var found []string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			found = append(found, c)
		}
	}
	return found, nil
}

// readPubKeys parses every authorized-key line from each given file. Comments
// and empty lines are skipped. Returns the parsed (key, marshaled-line) pairs.
type pubKey struct {
	parsed ssh.PublicKey
	line   string
}

func readPubKeys(paths []string) ([]pubKey, error) {
	var out []pubKey
	seen := map[string]bool{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		for rest := data; len(bytes.TrimSpace(rest)) > 0; {
			parsed, _, _, restAfter, err := ssh.ParseAuthorizedKey(rest)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", p, err)
			}
			fp := ssh.FingerprintSHA256(parsed)
			line := strings.TrimRight(string(bytes.TrimSpace(bytes.SplitN(rest, []byte{'\n'}, 2)[0])), " \t")
			if !seen[fp] {
				seen[fp] = true
				out = append(out, pubKey{parsed: parsed, line: line})
			}
			rest = restAfter
		}
	}
	return out, nil
}

// uploadKeyToOne dials the host and merges the keys into authorized_keys.
func uploadKeyToOne(cmd *cobra.Command, host *config.Host, keys []pubKey, policy sshx.HostKeyPolicy) error {
	ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
	defer cancel()
	c, err := sshx.Dial(ctx, host, policy)
	if err != nil {
		return err
	}
	defer c.Close()
	fc, err := sftp.NewClient(c.SSH())
	if err != nil {
		return err
	}
	defer fc.Close()

	added, total, err := mergeAuthorizedKeys(fc, keys, uploadKeyForce)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStderr(), "  %d key(s) added, %d already present (total %d)\n", added, len(keys)-added, total)
	return nil
}

// mergeAuthorizedKeys reads ~/.ssh/authorized_keys (creating it if missing),
// appends any of `keys` not already pinned by SHA256 fingerprint, and writes
// the result atomically with mode 0600. Returns (added, totalLinesAfter).
//
// `~` here means the SFTP server's working directory (typically the user's
// home — that's what stock sshd gives us).
func mergeAuthorizedKeys(fc *sftp.Client, keys []pubKey, force bool) (added, total int, err error) {
	if err := fc.MkdirAll(".ssh"); err != nil {
		return 0, 0, fmt.Errorf("mkdir ~/.ssh: %w", err)
	}
	// Best-effort: tighten perms. Fails on quirky servers; ignore.
	_ = fc.Chmod(".ssh", 0o700)

	existing, err := readRemote(fc, ".ssh/authorized_keys")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, 0, err
	}

	have := map[string]bool{}
	for rest := existing; len(bytes.TrimSpace(rest)) > 0; {
		parsed, _, _, restAfter, perr := ssh.ParseAuthorizedKey(rest)
		if perr != nil {
			// Couldn't parse a line — skip ahead one line and continue. We
			// don't reject the whole file; users put weird stuff in there.
			i := bytes.IndexByte(rest, '\n')
			if i < 0 {
				break
			}
			rest = rest[i+1:]
			continue
		}
		have[ssh.FingerprintSHA256(parsed)] = true
		rest = restAfter
	}

	var toAdd []pubKey
	for _, k := range keys {
		if !have[ssh.FingerprintSHA256(k.parsed)] {
			toAdd = append(toAdd, k)
		}
	}
	if len(toAdd) == 0 && !force {
		// Count lines for reporting.
		return 0, countLines(existing), nil
	}

	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte{'\n'}) {
		buf.WriteByte('\n')
	}
	for _, k := range toAdd {
		buf.WriteString(k.line)
		buf.WriteByte('\n')
	}

	if err := writeFileBytes(fc, ".ssh/authorized_keys", buf.Bytes()); err != nil {
		return 0, 0, err
	}
	_ = fc.Chmod(".ssh/authorized_keys", 0o600)

	return len(toAdd), countLines(buf.Bytes()), nil
}

func readRemote(fc *sftp.Client, path string) ([]byte, error) {
	f, err := fc.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func writeFileBytes(fc *sftp.Client, path string, data []byte) error {
	return writeFile(fc, path, bytes.NewReader(data))
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if !bytes.HasSuffix(b, []byte{'\n'}) {
		n++
	}
	return n
}
