# Guest agent

`dew-agent` is the daemon inside every guest. `initramfs/build.sh` bakes it in
at `/usr/local/bin/dew-agent`, and `init-stage2` starts it. The host talks to
it over vsock (see AGENTS.md, "Host ↔ Guest communication").

It is Rust, in the Cargo workspace under `rust/`:

- `rust/dew-proto`: the wire types. `internal/vsock` (Go, host side) is the
  source of truth; dew-proto mirrors it field for field, and golden frames
  keep the two in step (AGENTS.md, "Wire protocol parity").
- `rust/dew-agent`: the agent. Static `{aarch64,x86_64}-unknown-linux-musl`
  binaries linked with `rust-lld`, so they build from macOS or Linux without
  Docker. `rust/rust-toolchain.toml` pins the toolchain and the musl targets.

Until 0.9.5 the agent was written in Go (`cmd/dew-agent`). 0.9.5 moved
`minimal` to the Rust agent, and 0.9.6 every profile; the Go agent was then
removed (#53–#66).

## Tests

- `cargo test` in `rust/`: unit tests and the integration tests in
  `rust/dew-agent/tests/`, which drive exec, TTY, connect and expose over a
  socketpair.
- `rust/dew-agent/tests/confine.rs`: `--confine` enforcement, as root on
  Linux (it skips otherwise). CI runs it as root, inside the coverage step,
  which holds the line floor at 95%.
- `rust/dew-agent/testdata/seccomp`: recorded seccomp programs, with their
  inputs. The Rust test rebuilds every one, so the filters can't drift.
- The conformance suite, below.

## Conformance suite

`test/conformance` boots one image and drives its agent over real vsock
through the host's own code (`internal/vm/darwin`, `internal/vsock`). It
covers:

- handshake and auth, ping
- buffered exec: output, exit code, stderr only on failure, env/dir,
  several requests on one conn, 4 MiB output, missing command, timeout
- streaming exec: exit code, timeout, stdin, 20k+20k-line interleave stress
- TTY: isatty, size, resize, hangup
- connect and expose
- confine: uid drop, no_new_privs, read-only root, address families,
  fail-closed specs, streaming refused

```bash
# on a Mac of the image's arch (Apple VZ boots only the host's arch)
scripts/conformance.sh KERNEL INITRD                   # minimal
scripts/conformance.sh --disk-gb 4 KERNEL INITRD       # node/python/standard
scripts/conformance.sh --agent BIN KERNEL INITRD       # overlay a local agent
```

It can't run in CI: GitHub's macOS runners have no nested virtualization
(the Apple Silicon ones refuse every VM). CI instead builds `minimal` for
both arches and checks the baked agent (`initramfs-agent`), and
`kernel.yml` (on kernel changes, or run by hand) boots the x86_64 image.

Results (verified 2026-10-28): every profile passes 25 of 25 on both arches
(aarch64 on an M3, x86_64 on an i9 iMac), and so do the 0.9.5 minimal and
0.9.6 standard release images on aarch64.

## Behaviour worth knowing

Error text the host shows matches Go's standard library, for example
`timeout after 1.5s`, `exec: "x": executable file not found in $PATH` and
`dial tcp …: connect: connection refused`. `/etc/passwd` and `/etc/group`
are parsed with Go `os/user`'s rules.

- An exec timeout kills the command's whole process group, so a background
  child holding stdout open can't keep the exec past its deadline.
- Streamed output lines longer than 64 KiB are split into chunks, keeping
  line endings and UTF-8 intact. A trailing `\r` is stripped from streamed
  lines.
- Dropping the host side of a TTY session hangs the terminal up: SIGHUP to
  the session's process group, and the master is closed so a command that
  ignores SIGHUP gets EIO. In TTY mode an EOF frame only stops reading input
  (it doesn't send `^D`), a resize frame's data is ignored, and undecodable
  input is skipped.
- Connect and expose proxying don't half-close. A transient `accept` error
  on an exposed port (e.g. EMFILE) backs off and keeps the listener.
- An empty token, once set, authorizes empty-token requests.
- `--confine`:
  - A request asking for it on a streaming exec is refused (the host never
    sends one).
  - A `User=`/`Group=` id that is not a valid `uid_t`/`gid_t` is rejected
    (exit 126).
  - The agent's `DEW_EXEC_USER` reaches the shim as `DEW_CONFINE_EXEC_USER`,
    and a copy of either in the request env is ignored.
  - The command is resolved on, and inherits, the agent's pinned PATH; the
    request's PATH does not apply.
  - `SystemCallFilter=` names missing from the arch's syscall table are
    dropped, so a typo in a denylist blocks nothing. **aarch64 has no
    `mkdir`, only `mkdirat`**: a denylist naming just `mkdir` is a no-op on
    arm64 guests. The tables come from go-seccomp-bpf v1.6.0
    (`scripts/gen-seccomp-syscalls.py`).
