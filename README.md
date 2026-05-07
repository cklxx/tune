# tune (`tn`)

`tn` is a CLI for working against a remote SSH host — typically through a
jumpbox, with password or key auth — designed for both humans and coding
agents (Claude Code, Cursor, Aider, etc.).

It's pure-Go SSH (`golang.org/x/crypto/ssh` + `github.com/pkg/sftp`):
one static binary, no `ssh` / `sshpass` / `rsync` / `scp` on your `PATH`,
and one TCP connection to the jump that every subsequent operation is
multiplexed over.

## Why not just use `ssh`?

- **One binary, no system deps.** `go install` it on a fresh laptop or
  inside a container and everything works — no `sshpass` for password
  auth, no `rsync` for tree copies, no `~/.ssh/config` to coax through a
  jump host.
- **One connection through the jump.** Every `tn exec`, `tn read`,
  `tn ls`, `tn push` after the dial is a multiplexed SSH channel on the
  same `ssh.Client`. No per-command reconnect, no socket files.
- **Agent-friendly output by default.** Plain `key: value` for humans,
  `--json` everywhere it matters, atomic writes, real exit codes,
  stdout/stderr cleanly separated, error messages that say *how to fix
  it* (e.g. "auth failed — try `tn upload-key` after a password
  connect").
- **Reverse SOCKS5 in one command.** `tn proxy` lets a firewalled remote
  reach the public internet through *your* laptop's network, so
  `pip install`, `npm install`, `apt-get`, `git fetch` work on a box
  that otherwise can't reach PyPI or GitHub.

It's not (yet) a Mutagen-class bidirectional sync tool. For "an agent or
human shells out to a remote box, reads/writes some files, runs some
commands, maybe installs a package," it slots in cleanly.

## Quick start

```sh
$ tn init prod
host alias [prod]:
target addr (host[:port]): 10.0.0.42
target user: alice
target identity file (optional): ~/.ssh/id_ed25519
target passwordCmd (optional, e.g. 'pass show ssh/host'):
use a jump host? [y/N]: y
jump addr (host[:port]): jump.example.com
jump user: alice
jump identity file (optional):
jump passwordCmd (optional): pass show ssh/jump
saved host "prod" to /home/alice/.tn/config.yaml

$ tn status
host:      prod
target:    10.0.0.42
hasJump:   true
dialMs:    412
pingMs:    38
remote:    Linux 6.1.0 x86_64
            up 17 days, 4:22, ...
            /dev/sda1  ... 78% /home
ok:        true

$ tn exec -- uname -a
Linux 10-0-0-42 6.1.0 #1 SMP ...

$ tn push ./code /srv/app
$ tn pull /var/log/app.log ./logs/

$ tn read /etc/hostname
10-0-0-42

$ echo "FOO=bar" | tn write /tmp/env

$ tn upload-key             # graduate from password to key auth
uploading to target:
  1 key(s) added, 0 already present (total 1)
```

## Agent-friendly by design

Agent-friendliness isn't a side effect — it's the design contract every
subcommand obeys:

- **Scriptable output.** Plain mode is one `key: value` per line, no
  ANSI, no banners. `--json` (on `status`, `read`, `ls`, `bench`,
  `doctor`, `mirror`) returns line-delimited or single-doc JSON.
- **Stream separation.** Subcommand output goes to stdout; logs,
  prompts, and progress go to stderr. `tn read | jq` works; `tn exec --
  some-cmd | grep …` works.
- **Non-interactive paths exist.** Every prompt is skippable: passwords
  via `passwordCmd`, host keys via `--accept-new` /
  `TN_ACCEPT_NEW_HOSTKEYS=1`, host selection via `-H` / `$TN_HOST`,
  `tn shell` refuses to run without a TTY (use `tn exec` instead).
- **Atomic writes.** `tn write` and `tn upload-key` write to
  `<path>.tn-tmp` then `posix-rename` into place, so a concurrent
  reader sees old or new — never half. `tn upload-key` is idempotent
  (dedup by SHA-256 fingerprint).
- **Real exit codes.** `tn exec` propagates the remote process's exit
  code. `tn doctor` exits non-zero if any host failed. `tn exec --proxy`
  exits 2 (distinct from the inner command) when the proxy.env is
  stale or missing.
- **Hard timeouts everywhere.** `--timeout` on dial,
  `--timeout`/`--parallel` on `doctor`. No silent hangs.
- **Actionable errors.** `auth failed — try \`tn upload-key\` after a
  password connect`. `dial timed out — firewall blocking, wrong port,
  or VPN down?`. `server requires GSSAPI/Kerberos auth which tn does
  not support — use the system ssh to open a tunnel and point tn at
  the local port`. The error message tells you the next move.

## What it gives you

13 subcommands, all with `tn <cmd> --help` carrying examples and side
effects. The set:

- **`tn exec -- <cmd>`** — run a command on the remote, stream
  stdout/stderr verbatim, propagate the exit code, forward Ctrl-C as
  remote SIGINT. `--cwd`, `-e KEY=VALUE`, `--proxy` for the SOCKS5
  injection.
- **`tn push` / `tn pull`** — recursive file transfer over SFTP with
  concurrent reads/writes (`MaxConcurrentRequestsPerFile=64`).
- **`tn read` / `tn write` / `tn ls`** — agent-friendly file primitives.
  `tn write` is atomic. `tn ls --json` returns size, mode (octal),
  uid, gid, mtime, isDir.
- **`tn mirror <remote> <local>`** — one-way pull of a remote tree;
  skips files unchanged by `(size, mtime)`. `--once` for one-shot,
  default is poll-loop. `--include` / `--exclude` globs, optional
  `--delete`, `--workers` for parallel downloads. The "grep / IDE / LSP
  the remote codebase locally" path.
- **`tn proxy`** — reverse SOCKS5: opens a listener *on the remote*
  served by *your* network. Optionally drops `~/.tn/proxy.env` so
  `tn exec --proxy` picks it up; auto-reconnects on transient failures;
  exits cleanly on port-in-use or auth errors.
- **`tn shell`** — interactive PTY with SIGWINCH forwarding.
- **`tn status`** — dial time, ping RTT, remote `uname` + `df` summary.
- **`tn bench`** — measure cold dial cost, RTT distribution over N
  pings, no-op exec turnaround, single-stream throughput. Useful to
  decide whether the (planned) daemon mode is worth setting up for
  your link.
- **`tn doctor`** — probe every host in your config in parallel with a
  short timeout; non-zero exit if any failed. `--json` for monitoring.
- **`tn upload-key`** — `ssh-copy-id` equivalent. Reads your local
  `~/.ssh/id_{ed25519,ecdsa,rsa}.pub`, merges them into the remote's
  `~/.ssh/authorized_keys` deduped by fingerprint, atomic write, perms
  tightened to 0600/0700. `--jump` does the same on the jump host.
- **`tn init`** — interactive host setup. Re-running edits the entry
  rather than overwriting.
- **TOFU host-key pinning.** First connect prompts; subsequent connects
  verify against `~/.tn/known_hosts`. `--accept-new` to skip the prompt
  on trusted networks; `--insecure-host-key` for ad-hoc testing only.

## Letting the remote use your local network

In one terminal:

```sh
$ tn proxy
tn proxy: listening on remote 127.0.0.1:1080 — Ctrl-C to stop
  remote setup:
    export ALL_PROXY=socks5h://127.0.0.1:1080
```

In another:

```sh
$ tn exec --proxy -- pip install requests
```

`tn proxy` writes `~/.tn/proxy.env` on the remote by default; `tn exec
--proxy` sources it after probing the listener (no silent routing
through a dead port). Domain names resolve on your local side (that's
what `socks5h://` means), so private DNS visible only to your local
network works.

## Behind a Kerberos / GSSAPI jump host

Some corporate jump hosts (Kerberos-protected internal jumpboxes that
require `gssapi-with-mic`) reject everything else.
`golang.org/x/crypto/ssh` doesn't implement GSSAPI, so `tn` cannot dial
these jumps directly.

The escape hatch is to let your system `ssh` (which has GSSAPI) handle
the jump as a TCP forward, and point `tn` at the resulting local port:

```sh
# Terminal 1: run the GSSAPI'd tunnel. Reuse your existing ticket
# (kinit / klist) — system ssh handles the auth on the jump.
ssh -N -L 12222:target.internal:22 jump.corp.example.com

# Terminal 2: tn talks to the target via 127.0.0.1:12222 — no jump
# needed in tn's config.
cat > ~/.tn/config.yaml <<'YAML'
defaultHost: prod
hosts:
  prod:
    target:
      addr: 127.0.0.1:12222
      user: alice
      identityFile: ~/.ssh/id_ed25519
YAML

tn status
```

If you trust the network (loopback only, no MITM possible), add
`--accept-new` to skip the first-connect TOFU prompt:

```sh
tn --accept-new status         # pin the host key on first sight
TN_ACCEPT_NEW_HOSTKEYS=1 tn ls /etc   # same, via env
```

When `tn` detects the server only offers `gssapi-with-mic` it surfaces
this hint directly in the error message — you don't have to guess.

## Install

**One-liner** (Linux / macOS, prebuilt binary, falls back to `go install`
if no published release matches your platform):

```sh
curl -fsSL https://raw.githubusercontent.com/cklxx/tune/main/install.sh | sh
```

Pin a version or override the install directory:

```sh
VERSION=v0.1.0 INSTALL_DIR=$HOME/.local/bin \
  sh -c "$(curl -fsSL https://raw.githubusercontent.com/cklxx/tune/main/install.sh)"
```

**With Go**:

```sh
go install github.com/cklxx/tune/cmd/tn@latest
```

**From source**:

```sh
git clone https://github.com/cklxx/tune
cd tune
make build       # writes ./tn
```

**Windows**: download the `.zip` from
[the latest release](https://github.com/cklxx/tune/releases/latest)
and unpack `tn.exe` somewhere on your `PATH`.

## Config

`~/.tn/config.yaml` (override with `$TN_HOME`):

```yaml
defaultHost: prod
hosts:
  prod:
    target:
      addr: 10.0.0.42:22
      user: alice
      identityFile: ~/.ssh/id_ed25519
      # passwordCmd: pass show ssh/prod
    jump:
      addr: jump.example.com:22
      user: alice
      passwordCmd: pass show ssh/jump
```

Auth precedence per hop, in order:

1. `identityFile` (if set and parseable).
2. `SSH_AUTH_SOCK` agent (if reachable).
3. `passwordCmd` output (any shell command — works with `pass`, `op read`,
   `security find-generic-password`, etc.).
4. Interactive prompt (terminal only).

`tn` never writes plaintext passwords to disk.

## For coding agents: structured output

Pass `--json` to `read`, `ls`, `status`, `bench`, `doctor`, `mirror` for
structured output:

```sh
$ tn ls --json /etc | jq '.[0]'
{"name":"adduser.conf","size":3026,"mode":"0644","mtime":1714632812,"isDir":false}

$ tn read --json /etc/hostname | jq -r '.content'
10-0-0-42

$ tn status --json | jq '.ok'
true
```

`tn exec` always streams stdout/stderr verbatim and propagates the remote
exit code, so it composes cleanly with shell pipelines and agent tool
harnesses.

## Releasing

Tag-driven via GoReleaser. To cut a new release:

```sh
git tag v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` runs vet + tests, then GoReleaser builds
the cross-platform matrix (linux/darwin/windows × amd64/arm64, sans
windows-arm64), uploads tar.gz/zip archives + `checksums.txt` to a
GitHub Release, and renders an install snippet in the release notes.

## Development

```sh
make build    # build ./tn
make test     # run go test ./... — fully hermetic (no real ssh required)
make vet      # static analysis
```

The test suite is end-to-end without a real SSH server or network egress:
`internal/sshtest` boots in-process SSH servers (session/exec, SFTP
subsystem, `direct-tcpip`, `tcpip-forward`) on a free port with
generated ed25519 host keys, and the dial path, jump path, host-key
rejection, reverse SOCKS5, atomic writes, push/pull, list, and bench
all have integration coverage that runs in milliseconds.

## Roadmap

- **Persistent daemon (`tn daemon`)** — per-host Unix-socket daemon that
  holds the SSH client open, so subsequent `tn exec` / `tn read` etc.
  cost ~5ms instead of 200-500ms. CLI auto-attaches if running, falls
  back to direct dial otherwise. (Foundation laid; not yet shipped.)
- **Native rsync-style delta sync** — block-level diff for large
  files in `tn push` / `tn mirror`.
- **Connection-level auto-reconnect with op replay** — currently `tn
  proxy` reconnects on its own; a generic `sshx.Client` redial would
  also help long-running shells and exec.
- **Optional remote agent (`tnd`)** — small Go binary auto-uploaded to
  the remote (à la Mutagen) that turns 50ms-per-stat into 1ms by
  batching syscalls. Required for a usable bidirectional `tn mirror`.
- **Bidirectional mirror** — local edits propagate up; remote edits
  propagate down; conflict resolution. (Today's `tn mirror` is one-way.)

See [docs/comparison.md](docs/comparison.md) for how `tn` lines up
against Mutagen, distant, Eternal Terminal, and friends.

## License

MIT.
