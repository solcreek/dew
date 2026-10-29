# Rust guest agent migration

Status: done. Phases 1–5 merged (#53, #54, #56, #57, #59, #60). 0.9.4 shipped the
Go agent's `--confine` fixes (#58) on their own. Phase 6, step one (#62):
0.9.5 ships the Rust agent in the minimal profile. The 0.9.5 aarch64
minimal image passes the conformance suite (25 pass, verified 2026-10-28).
Step two, the Rust agent for every profile, merged in #65. It did not wait
a release after 0.9.5 as planned: dew has few users yet, so the owner chose
to move on. Phase 7 removed the Go agent (#66). 0.9.6 is the first release
with the Rust agent in every profile and no Go agent (2026-10-28).

## Why the agent, and only the agent (for now)

A POC compared Go and Rust for both halves of dew on an M3, booting the
shipped kernel and minimal initramfs (2026-10-28):

| | Go agent | Rust agent |
|---|---:|---:|
| static binary | 3.67 MB | 0.59 MB |
| minimal initramfs | — | −1.1 MB compressed (~16 %) |
| RSS after workload | 9.1 MB | 0.57 MB |
| peak RSS | 25.8 MB | 8.2 MB |
| streaming exec, 200k lines | 1760 ms | 689 ms |

Boot-to-ready and fork/exec latency were the same in both languages: the
guest kernel, init and fork dominate. On the host side, a Rust CLI changed
nothing a user waits for, and a port would cost 31k lines, ~500 tests, and
Go's ~1 s incremental builds. So the host stays in Go unless the host track
below is chosen deliberately later.

The agent gains land where dew is constrained: inside a 512 MB minimal VM,
and under `--cgroup`, where the agent shares the workload's memory cap. A
smaller agent is less likely to be the process the OOM killer picks.

## Ground rules

- **The wire protocol does not change** during the migration. Go host ↔ Rust
  agent and Rust host ↔ Go agent must both work at every phase. The POC ran
  that 2×2 matrix and it passed in every cell.
- **The Go agent stays the default** until the Rust agent passes the same
  tests and the conformance suite (phase 5) on both arches.
- **Every phase ships as its own PR**, merges to `main` and leaves `main`
  releasable. Nothing user-visible changes before phase 6.
- Rust lives in a Cargo workspace under `rust/`, so the Go module at the repo
  root is untouched (`go test ./...` does not see it).
- The guest target is `{aarch64,x86_64}-unknown-linux-musl`, statically
  linked with `rust-lld`. It builds from macOS without Docker or zig.

## Phases (one PR each)

| # | PR | Contents | Done when |
|---|---|---|---|
| 1 | **proto + CI** | `rust/dew-proto`: serde mirror of `internal/vsock` (all message types, `omitempty` parity, single-write framing). Go-generated golden frames in `internal/vsock/testdata/golden/`, decoded and re-encoded by both Go and Rust tests. CI job: fmt, clippy `-D warnings`, test. This doc. | Both languages agree on every golden frame |
| 2 | **agent core** | `rust/dew-agent`: vsock listener, set_token (fail-closed auth), buffered exec, streaming exec (+stdin), connect proxy, ping, `DEW_VSOCK_PORT`, `DEW_EXEC_USER`, PATH pin, periodic `sync(2)`. Port the Go agent tests that cover these. CI cross-builds both musl targets and records sizes. | Unit tests pass on Linux CI; binary not shipped |
| 3 | **TTY + exposes** | PTY exec (base64 frames, winch), set_exposes listeners on 127.0.0.2 and the reverse dial to the host on vsock port 1025. Port `pty_test`. | Feature parity with the Go agent except `--confine` |
| 4 | **confine** | Re-exec shim: mount namespace + read-only root + read-write binds, uid/gid/DynamicUser drop, capability bounding/keep/drop, no_new_privs, seccomp (address families, explicit syscalls). Port `caps_test`, `seccomp_test`. Only then does the agent ack `confine: true` in the set_token reply. | Parity with `docs/confine-enforcement.md` |
| 5 | **build switch + conformance** | `initramfs/build.sh` gains `DEW_AGENT=go\|rust` (default `go`), plus a drift-guard test for it. A conformance suite (grown from the POC harness) boots each profile with each agent and runs the same scenarios: exec, stream, stdin, TTY, connect, expose, confine, interleave stress. The release dry-run builds both. | Suite green for both agents on both arches |
| 6 | **ship** | Release builds the Rust agent. Default it for `minimal` first, one release later for every profile. Keep the Go agent buildable as a fallback. CHANGELOG entry with size and memory numbers. | One release with no agent regressions |
| 7 | **retire** | Delete `cmd/dew-agent` and its Linux-only CI job. Code the host needs (`internal/vsock`) stays; `internal/agentauth` and `internal/guestenv` turned out to be agent-only and go too. The seccomp goldens move to `rust/dew-agent/testdata`. | — |

Phases 2–4 can be reviewed in parallel once phase 1 merges, since they touch
separate modules of the crate.

## Conformance suite (phase 5)

`test/conformance` boots one image and drives its agent over real vsock
through the host's own code (`internal/vm/darwin`, `internal/vsock`). The
same tests run against either agent, which they detect from the running
binary. Covered: handshake and auth, ping, buffered exec (output, exit
code, stderr only on failure, env/dir, several requests on one conn, 4 MiB
output, missing command, timeout), streaming exec (exit code, timeout,
stdin, 20k+20k-line interleave stress), TTY (isatty, size, resize, hangup),
connect, expose, and confine (uid drop, no_new_privs, read-only root,
address families, fail-closed specs, streaming refused). A case where the
agents differ on purpose (the table below) skips its Go run.

```bash
# on a Mac of the image's arch (Apple VZ boots only the host's arch)
scripts/conformance.sh KERNEL INITRD                   # minimal
scripts/conformance.sh --disk-gb 4 KERNEL INITRD       # node/python/standard
scripts/conformance.sh --agent BIN KERNEL INITRD       # overlay a local agent
```

It can't run in CI: GitHub's macOS runners don't support nested
virtualization. Results with images from `initramfs/build.sh` at 032156f
(aarch64 on an M3, x86_64 on an i9 iMac, verified 2026-10-28):

| | minimal | standard (4 GB disk) |
|---|---|---|
| aarch64, Go | 22 pass, 3 skip | 22 pass, 3 skip |
| aarch64, Rust | 25 pass | 25 pass |
| x86_64, Go | 22 pass, 3 skip | 22 pass, 3 skip |
| x86_64, Rust | 25 pass | 25 pass |

For phase 6 step two, node, python and standard built with the default
agent (Rust) at 6b3ad78 each pass 25 on both arches, with a 4 GB disk
(verified 2026-10-28). On aarch64 standard, `dew run` also ran node, npm,
crun and a `--with redis` service (PONG).

The suite also fails where it should:
a Go agent built from before #44 fails the interleave stress on its first
run, and the standard image of release 0.9.1, whose agent predates #44 and #58,
fails the interleave stress and the uid-range case. Instead of a release
dry-run, the `initramfs-agent` CI job (#59) builds minimal with each agent
on both arches.

## Host track (optional, decide after phase 6)

Not planned. Reopen only with a concrete driver, for example:

- **vmnet networking.** `objc2-virtualization` already exposes
  `VZVmnetNetworkDeviceAttachment`, which Code-Hex/vz does not yet have
  (see `internal/vm/darwin`). Try a small cgo shim in Go first.
- **Shared code.** If Rust host code would actually share types with the
  agent beyond `dew-proto`.

If it happens, the order would be: `internal/vm/darwin` behind a C ABI, then
leaf packages (`detect`, `services`, `dewfile`), and `cmd/dew` last. Every
step would sit behind the existing Go interfaces.

## Deliberate behaviour differences from the Go agent

The port matches the Go agent's wire shapes exactly (golden frames) and its
user-visible error wording where the host shows it, such as
`timeout after 1.5s`, `exec: "x": executable file not found in $PATH` and
`dial tcp …: connect: connection refused`. It differs on purpose in these
cases, and each one has a test in `rust/dew-agent/tests/`:

| case | Go agent | Rust agent | why |
|---|---|---|---|
| exec timeout, grandchild holds stdout (`sleep 9 & echo`) | kills only the direct child, then waits for the pipe, so the exec overruns its deadline | kills the child's process group; drains pipes before reaping | a deadline should end the exec |
| output line > 64 KiB | `bufio.Scanner` stops reading the pipe and the command stalls | splits into ≤ 64 KiB chunks, keeping line endings and UTF-8 intact across them | no stall |
| host disconnects from a TTY session | the shell keeps running (idle: the output loop never notices; busy: Wait blocks forever) — one leaked session per dropped conn, reproduced in a guest | SIGHUP to the session's process group when the conn fails before an EOF frame or an output write fails, and every copy of the master is closed so the line really hangs up and a command ignoring SIGHUP gets EIO: on the input side the feeder drops its dup and wakes the output loop (poll on the master + a wake socket on Linux; select on macOS, whose poll lacks tty support) to close the other; on the output side the conn is shut down so the feeder drops its dup. Not covered: a feeder blocked writing into a full input queue of a child that ignores SIGHUP and never reads | a dropped line hangs a terminal up |
| expose `accept` fails transiently (e.g. EMFILE; tested with an injected EMFILE acceptor) | the accept loop returns, leaving a dead listener in the map | backs off 50 ms and keeps the listener | the port keeps working |
| `--confine` on a streaming exec (the host refuses this combination) | the streaming path ignores the spec and runs the command **unconfined** | `confine:` error, nothing runs | fail closed |

Fixed in the Go agent instead, because each let a confined target keep root
(both agents now behave the same, and `tests/confine.rs` checks it):

- A `User=`/`Group=` id that is not a valid `uid_t` (`4294967295`,
  `4294967296`, …) is rejected (exit 126). It used to reach `setresuid`,
  where `(uid_t)-1` means "unchanged" and wider values wrap
  (`4294967296` → 0).
- The agent's `DEW_EXEC_USER`, the confined fallback identity, reaches the
  shim as `DEW_CONFINE_EXEC_USER`, with any copy in the request env
  stripped (as `DEW_CONFINE_SPEC` already was), and is unset before the
  target runs. The Go shim used to read `DEW_EXEC_USER` from the merged env,
  where a request's env could override it, e.g. with `0`.

Kept identical even where questionable, so the migration is not also a
behaviour change:

- An empty token, once set, authorizes empty-token requests.
- Connect and expose proxying don't half-close.
- A trailing `\r` is stripped from streamed lines.
- In TTY mode, an EOF frame only stops reading input; it doesn't send `^D`.
- A resize frame's data is ignored, and undecodable input is skipped.
- Confined execs resolve the command on, and inherit, the agent's pinned
  PATH: the request's PATH does not apply (the Go agent pins PATH before its
  shim runs).
- `SystemCallFilter=` names missing from the arch's syscall table are
  dropped, so a typo in a denylist blocks nothing. Note **aarch64 has no
  `mkdir`, only `mkdirat`**: a denylist naming just `mkdir` is a no-op on
  arm64 guests. The tables are generated from go-seccomp-bpf, and every
  filter program is checked against Go-emitted goldens
  (`rust/dew-agent/testdata/seccomp`, moved from `cmd/dew-agent` in phase 7).
- `/etc/passwd` and `/etc/group` are parsed with Go `os/user`'s rules.

## Risks

- **Release CI gains a Rust toolchain.** Pin it in `rust/rust-toolchain.toml`.
- **Two agents for a while.** The conformance suite is what keeps them
  honest. Do not let phase 5 slip behind phases 6–7.
- **Behaviour drift.** For example: exec env construction, timeout error
  strings, the exit code reported for a signalled process. The golden frames
  cover the shapes, and the conformance suite covers the semantics.
