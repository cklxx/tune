# tune (`tn`)

`tn` is a CLI for working against a remote SSH host — typically through a
jumpbox, with password or key auth — designed for both humans and coding
agents (Claude Code, Cursor, Aider, etc.).

It uses pure-Go SSH (`golang.org/x/crypto/ssh` + `github.com/pkg/sftp`), so
there is no dependency on the system `ssh`, `sshpass`, or `rsync` binaries.

## What it gives you

- **One TCP connection through the jump.** A single `ssh.Client` to the jump,
  a `direct-tcpip` channel to the target, an SSH handshake on top — every
  subsequent operation is a multiplexed SSH channel. No OS-level processes,
  no socket files, no `sshpass`.
- **`tn exec`** — run a command on the remote, stream stdout/stderr,
  propagate exit code, forward Ctrl-C as a remote SIGINT.
- **`tn push` / `tn pull`** — recursive file transfer over SFTP with
  concurrent reads/writes.
- **`tn read` / `tn write` / `tn ls`** — agent-friendly file primitives.
  Atomic writes (tmp + `posix-rename`). `--json` for structured output.
- **`tn proxy`** — *reverse* SOCKS5: opens a listener on the remote, serves
  SOCKS5 from your local box. The remote then uses your local network to
  reach the public internet, so `pip install`, `npm install`, `apt-get`,
  `git fetch`, etc. work even when the remote is firewalled.
- **`tn shell`** — interactive PTY with window-resize forwarding.
- **`tn status`** — dial time, ping RTT, remote `uname` + `df` summary.
  `--json` for monitoring.
- **`tn bench`** — measure dial cost, RTT distribution over N pings,
  per-call exec turnaround, and single-stream throughput. Useful to decide
  whether to spin up the (planned) daemon mode.
- **`tn upload-key`** — ssh-copy-id equivalent. Reads your local public
  keys (defaults to ed25519/ecdsa/rsa under `~/.ssh`), merges them into
  the remote's `~/.ssh/authorized_keys` deduped by SHA256 fingerprint
  (so re-running is a no-op), atomic write, perms tightened to 0600/0700.
  `--jump` does the same on the jump host. Run once after the first
  password connect to graduate to key auth.
- **TOFU host-key pinning.** First connect prompts; subsequent connects
  verify against `~/.tn/known_hosts`.

## Install

```sh
go install github.com/cklxx/tune/cmd/tn@latest
```

Or build from source:

```sh
git clone https://github.com/cklxx/tune
cd tune
go build -o tn ./cmd/tn
```

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
enable transport compression? [y/N]: y
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
--proxy` sources it before running your command. Domain names are resolved
on your local side (that's what `socks5h://` means), so private DNS visible
only to your local network works.

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

## For coding agents

Pass `--json` to `read`, `ls`, and `status` for structured output:

```sh
$ tn ls --json /etc | jq '.[0]'
{"name":"adduser.conf","size":3026,"mode":"0644","mtime":1714632812,"isDir":false}

$ tn read --json /etc/hostname | jq -r '.content'
10-0-0-42
```

`tn exec` always streams stdout/stderr verbatim and propagates the remote
exit code, so it composes cleanly with shell pipelines and agent tool
harnesses.

## Development

```sh
make build    # build ./tn
make test     # run go test ./... — fully hermetic (no real ssh required)
make vet      # static analysis
```

The test suite includes end-to-end coverage that does not need a real SSH
server or network egress:

- `internal/sshtest` boots in-process SSH servers (`session`/`exec`,
  `subsystem sftp`, `direct-tcpip`, `tcpip-forward`) on a free port using
  generated ed25519 host keys.
- `sshx_test.TestDialDirect` / `TestDialThroughJump` / `TestDialRejectsBadKey`
  cover the dial path and auth.
- `sshx_test.TestReverseSocksThroughSSH` is the killer integration test:
  boots a target SSH server, opens a `tcpip-forward` listener, runs the
  real `socks.Serve` over the channels, and round-trips bytes through a
  local "echo internet" listener — exactly the path `tn proxy` exercises.
- `cli_test.TestPushPullRoundTrip` / `TestWriteFileAtomicity` /
  `TestReadFilePlainAndJSON` / `TestList` exercise SFTP via a real
  `pkg/sftp` server rooted at `t.TempDir()`.
- `cli_test.TestBenchEndToEnd` runs the bench command's logic against an
  in-process server with a stdin-draining exec handler.

## Roadmap

- Persistent daemon (`tn daemon`) — sub-10ms per-call latency by avoiding
  TCP/SSH handshakes on each invocation.
- Native rsync-style delta sync.
- Auto-reconnect with op replay.
- Optional remote agent (`tnd`) for batched syscalls and watch-mode mirror.

## License

MIT.
