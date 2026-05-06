# How tn compares to similar tools

Quick positioning for people coming from other remote-development CLIs. Last
updated 2026-05.

## Summary

| Tool                | Language | Pure-Go/Rust SSH | Jumpbox auth in tool | Reverse SOCKS | File sync     | Remote agent  | Daemon        | Auto-reconnect |
| ------------------- | -------- | ---------------- | -------------------- | ------------- | ------------- | ------------- | ------------- | -------------- |
| **tn** (this)       | Go       | yes              | yes (incl. password) | yes (1st-class) | one-shot SFTP | (planned)     | (planned)     | (planned)      |
| Mutagen             | Go       | yes              | via OpenSSH config   | yes           | bidirectional, watch | auto-installed | yes      | yes            |
| distant             | Rust     | yes              | via OpenSSH config   | no            | one-shot      | yes (manual)  | yes (manager) | partial        |
| Eternal Terminal    | C++      | no (libssh)      | tmux-style only      | tunnels only  | no            | yes           | yes           | yes (its core) |
| Mosh                | C/C++    | no               | no                   | no            | no            | yes (mosh-server) | n/a       | yes (UDP roam) |
| sshuttle            | Python   | no (system ssh)  | no                   | reverse VPN (other direction) | no | yes (boot)  | runs in fg    | no             |
| chisel              | Go       | n/a (HTTP tunnel)| no                   | yes           | no            | yes           | yes           | yes            |
| gofs                | Go       | yes              | no                   | no            | watch sync    | no            | yes           | n/a            |
| `ssh-copy-id`       | shell    | n/a              | OpenSSH config       | no            | one file      | no            | no            | n/a            |

## Where tn fits

`tn`'s niche is **fast first-time bootstrap onto a password-protected,
jumpbox-fronted host**, with **agent-friendly stdio** for AI coding tools.
Mutagen is more powerful for sustained file-sync workflows; distant is more
powerful for in-editor remote development; ET is more powerful for terminal
roaming. None of them prioritize the "I just got a credential to a jumpbox,
let me start working immediately" path.

Specifically:

- **You don't need OpenSSH on the local machine.** Mutagen and distant lean
  on `~/.ssh/config` for jump-host plumbing; `tn` carries jumpbox + password
  + identity-file + `passwordCmd` natively in YAML.
- **Reverse SOCKS5 is a single subcommand.** Most tools either don't expose
  it (Mutagen/distant), require chaining (`ssh -R 1080`), or are entire
  separate tools (chisel, sshuttle).
- **Structured output for agents.** `--json` on `read`, `ls`, `status`,
  `bench` produces stable parseable shapes. Mutagen and distant target
  humans/IDEs first.

## Where tn isn't (yet) competitive

- **No daemon.** Every CLI invocation does a fresh TCP+SSH handshake. For
  per-call latency below ~50ms you need a connection-holding process. Both
  Mutagen and distant have this. `tn bench` exists so you can measure
  whether it actually matters for you.
- **No file watch.** Mutagen's killer feature — bidirectional, conflict-aware,
  inotify-driven — is absent. `tn push` / `pull` are one-shot.
- **No remote agent.** Mutagen drops a binary on the remote (via scp) and
  uses it for batched syscalls. We rely on stock `sshd` + `pkg/sftp`'s
  subsystem. Slower per-call but zero install on the remote.
- **No auto-reconnect with op replay.** ET and Mosh make flaky links
  invisible. We retry only at the operation level (`tn` is one-shot, run
  it again).

## What we're stealing from each, when

- From **Mutagen**: file-watch + bidirectional mirror (planned `tn mirror`).
  Their wire protocol over a single `session+exec` channel is a great
  precedent.
- From **distant**: per-host manager with a small JSON-RPC over Unix socket;
  CLI invocations are RPC calls. (Planned `tn daemon`.)
- From **Eternal Terminal**: server-side process that survives client
  reconnects. We probably won't go this far — TCP-replay at the SSH level
  is enough for our scope.
- From **Mosh**: predictive local echo. Out of scope.
- From **chisel/sshuttle**: nothing — our reverse SOCKS already covers the
  "remote uses local network" use case directly.
- From **ssh-copy-id**: implemented as `tn upload-key` (idempotent, jumpbox
  aware).

## When you should not use tn

- You want full IDE integration. Use **Mutagen + your editor's SFTP plugin**
  or **distant.nvim**.
- You need offline-tolerant terminals (laptop closes, IP roams). Use
  **Mosh** or **Eternal Terminal**.
- You manage hundreds of nodes in parallel. Use **pdsh / clush / ansible**.
- You have OpenSSH everywhere and nothing else. Just use `ssh -J` and
  `rsync`.

## When you should reach for tn

- You have a coding agent that needs to read files, run commands, and pipe
  packages from your local network into the remote — and you want to set
  it up in 60 seconds without editing `~/.ssh/config`.
- You're behind a corporate jumpbox with password auth and want to graduate
  to keys without remembering the `ssh-copy-id -J` incantation.
- You like single static binaries.
