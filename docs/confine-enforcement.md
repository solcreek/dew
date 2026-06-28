# Design: agent-side `--confine` enforcement (R2 follow-up)

Status: partly implemented. The **read-only filesystem** (`ProtectSystem=strict`
+ `ReadWritePaths=`, §3) and the **native capability/uid/no_new_privs drop**
(§4) are both implemented via the agent-side shim described below. The seccomp
syscall allowlist (§5) remains to do. This doc tracks the shared substrate and
the remaining work.

Context: field notes (`hostd.sh/docs/dew-test-notes.md` §7b) confirm 0.8.x
validates the **resource-limit** half of a hardened unit (cgroup `MemoryMax` /
`TasksMax` / `CPUQuota`). Now also enforced, all in the agent shim:
`ProtectSystem=strict` + `ReadWritePaths=` (read-only fs, mount namespace — §3)
and `User=`/`Group=`/`DynamicUser=`, `CapabilityBoundingSet=`,
`NoNewPrivileges=` (native `prctl`/`capset`/`setresuid`, replacing the former
`setpriv` shell-out — §4). Still unenforced:
`SystemCallFilter=`/`RestrictAddressFamilies=` (seccomp) and the W^X /
namespace-restriction directives — surfaced as `--confine` warnings
(`confine.Plan.Unsupported`).

---

## 1. Why move enforcement into the guest agent

Today `--confine` enforces a unit in two disjoint ways (`cmd/dew/main.go`):

| Half | Channel | Where applied |
|---|---|---|
| cgroup limits | kernel cmdline `dew.*` (`appendGuestParams`) | init-stage2, before agent start |
| uid + caps drop | `Confinement` spec over vsock (§4) | agent shim, natively, before exec |
| read-only fs | `Confinement` spec over vsock (§3) | agent shim, mount namespace, before exec |

The uid/caps drop used to be a host-built `setpriv` prefix wrapped onto the argv;
it is now applied natively by the shim (§4), the same place the read-only fs and
(future) seccomp are set up. These per-exec, child-side operations **cannot** be
wrapped onto an argv from the host:

- **read-only fs** needs a mount namespace set up *in the child* before exec.
- **seccomp** needs a BPF program installed *in the child* before exec
  (`setpriv` can install a pre-built BPF blob via `--seccomp-filter`, but we'd
  still have to compile the `SystemCallFilter=` set into BPF — there's no
  systemd-group expansion in `setpriv`).

Both are per-exec, child-side operations. The natural owner is the **guest
agent**, which already sets `SysProcAttr.Credential` for the `DEW_EXEC_USER`
path. So the design moves confinement from "host wraps argv" to "host ships a
declarative confinement spec; agent applies it natively before exec." This
also subsumes the field notes' suggestion to drop caps via `PR_CAPBSET_DROP`
instead of shelling to `setpriv`, and makes confinement work on **every**
profile (even `minimal`, which has no util-linux).

---

## 2. Shared substrate: a confinement spec over vsock

Extend `ExecRequest` (`internal/vsock/protocol.go`) with one optional field:

The shipped wire type (`internal/vsock/protocol.go`) — uid/group are strings
(uid-or-name, resolved guest-side) and seccomp is not represented yet:

```go
type Confinement struct {
    // Privilege drop — carried but enforced by the setpriv prefix for now.
    User, Group string   // uid-or-name / gid-or-name; "" = unchanged
    DynamicUser bool     // no User= but DynamicUser=yes → fixed unprivileged uid
    NoNewPrivs  bool     // PR_SET_NO_NEW_PRIVS (planned native step)
    DropAllCaps bool     // empty/positive bounding set → drop all but KeepCaps
    KeepCaps    []string // caps retained when DropAllCaps
    DropCaps    []string // caps removed from the inherited set otherwise
    // Filesystem — applied by the agent today.
    ReadOnlyRoot   bool     // ProtectSystem=strict → remount / read-only
    ReadWritePaths []string // bind-rw exceptions (ReadWritePaths=)
    // Seccomp: not a field yet — design-only (§5).
}

type ExecRequest struct {
    // … existing fields …
    Confine *Confinement `json:"confine,omitempty"`
}
```

The host builds `*Confinement` from `confine.Plan`. As shipped, only the
read-only-fs fields are populated and applied by the agent; uid/caps still ride
the host-built `setpriv` prefix, so `confinementFromPlan` (in `cmd/dew`) returns
nil unless `ProtectSystem=strict`. The native uid/caps drop (which fills the
priv fields and removes the setpriv prefix) is the follow-up in §4. Import
direction: `internal/vsock` must not import `internal/confine` — the wire type
lives in `internal/vsock` and the `Plan → Confinement` mapping in `cmd/dew`.

Backward-compat: `Confine` is `omitempty`; an old agent ignores it (so a
newer host must not *rely* on it silently — gate on the agent handshake/version
if we add one, otherwise document that `--confine`'s fs/seccomp halves need a
guest built from the same release, which the asset-SHA pinning already ensures).

### The fork-exec constraint (key implementation detail)

Go does **not** let you run arbitrary code in the child between `fork` and
`exec` (it's not async-signal-safe). `syscall.SysProcAttr` can do *some* of
this declaratively:

- uid/gid → `Credential{Uid,Gid}` (already used)
- mount namespace → `Cloneflags: CLONE_NEWNS`
- ambient caps → `AmbientCaps` (Go 1.x supports it)

…but it **cannot** remount filesystems, drop the bounding set
(`PR_CAPBSET_DROP`), set `no_new_privs`, or install a seccomp filter — those are
imperative steps that must run in the child post-clone, pre-exec.

**Decision: a re-exec shim.** The agent re-execs *itself* with argv
`[exe, --dew-confine-shim, targetCmd, args...]` (the `--dew-confine-shim`
sentinel; see `cmd/dew-agent/confine.go`), the spec passed in the
`DEW_CONFINE_SPEC` env var, and the namespace flags in `SysProcAttr`; the shim
process (now inside the new mount ns) performs the imperative steps in order,
then `exec`s the real target. This is the standard pattern (runc/crun do the
same). The shim stays in the static no-cgo agent binary, so no extra asset.
Order inside the shim:

1. `unshare`-side already done by parent's `Cloneflags` (mount ns).
2. make mounts private (`mount("", "/", "", MS_REC|MS_PRIVATE, "")`).
3. read-only root + rw binds (§3).
4. `PR_SET_NO_NEW_PRIVS` (required before seccomp without CAP_SYS_ADMIN).
5. `PR_CAPBSET_DROP` for each dropped cap (§4).
6. install seccomp filter (§5, Phase 3).
7. `setresgid`/`setresuid` (after caps/seccomp so the drop itself is allowed).
8. `exec` target argv.

All steps are pure-Go via `golang.org/x/sys/unix` (already an indirect dep) —
`CGO_ENABLED=0` preserved.

---

## 3. Read-only fs (`ProtectSystem=strict` + `ReadWritePaths=`) — implementable now

systemd semantics we approximate:

- `ProtectSystem=strict` → the whole rootfs read-only, **except** `/dev`,
  `/proc`, `/sys` (API filesystems) and the explicit `ReadWritePaths=`.
- `ReadWritePaths=` → those paths stay writable.
- (`ProtectHome=`, `PrivateTmp=` are follow-ups; start with strict + RW paths.)

Shim implementation as shipped (inside the new mount ns):

1. `mount(MS_REC|MS_PRIVATE)` on `/` so changes don't propagate to the host
   view / other execs.
2. Materialize any missing `ReadWritePaths` (while still writable). A **missing**
   entry is always created as a *directory* (the state-directory case, e.g.
   `/var/lib/app`): file-vs-directory intent can't be inferred from a
   non-existent path. An **existing** entry is left as-is and bound by type in
   step 5 — so a writable *file* exception (e.g. `/etc/hostname`) must already
   exist on the rootfs to be bound as a file. Verified in-VM: existing file →
   writable; missing dir → created + writable; a missing *file* path becomes a
   writable directory (name an existing file, or its parent directory, instead).
3. `mount("/", "/", "", MS_BIND|MS_REC, "")` — recursively bind `/` onto itself.
   This is required so the per-mount remount in step 4 has a bind mount to
   operate on (and submounts are preserved); it also keeps the read-only change
   **per-mount**. A plain `MS_REMOUNT|MS_RDONLY` (no `MS_BIND`) is wrong here —
   superblock flags aren't namespaced, so it would flip the root filesystem
   read-only for the host/agent too.
4. `mount("", "/", "", MS_BIND|MS_REMOUNT|MS_RDONLY, "")` to flip the root mount
   read-only **non-recursively** (no `MS_REC`). Deliberate: submounts like
   `/proc`, `/sys`, `/dev`, `/tmp`, `/run`, the cgroup mount and virtiofs
   `--share` dirs are separate mounts and keep their own (writable) flags —
   exactly as systemd leaves API filesystems writable under
   `ProtectSystem=strict`. No re-binding of those is needed.
5. For each `ReadWritePaths` entry on the root fs: bind-mount it onto itself
   (`MS_BIND`, plus `MS_REC` only for directories — a recursive bind of a file
   is invalid) then `MS_BIND|MS_REMOUNT` without `MS_RDONLY` to restore write
   access. Binding an existing file path directly is a **dew-specific
   refinement** — systemd instead resolves a non-directory `ReadWritePaths=`
   entry via its closest existing ancestor directory. The file must exist (see
   step 2); a missing entry was materialized as a directory above.

Acceptance (locally testable on `standard`):

```
dew run --confine ro.service -- sh -c 'echo x > /etc/probe; echo rc=$?'   # EROFS, rc!=0
dew run --confine ro.service -- sh -c 'echo x > /var/lib/app/probe; echo rc=$?'  # rc=0 (RW path)
```

Effort: medium. Self-contained, pure Go, **fully verifiable in-VM**.

---

## 4. Native capability/uid drop (replace `setpriv` shell-out) — IMPLEMENTED

The host-side `wrapWithSetpriv` + util-linux dependency is gone; the shim applies
the drop natively (`cmd/dew-agent/caps.go`, `applyPrivilegeDrop`). Mapping:

- `CapabilityBoundingSet=` (empty / positive list) → `DropAllCaps`; drop every
  cap via `PR_CAPBSET_DROP` except `KeepCaps`.
- `CapabilityBoundingSet=~CAP_X` → `DropCaps`; drop only those.
- `NoNewPrivileges=yes` → `PR_SET_NO_NEW_PRIVS`.
- `User=`/`Group=`/`DynamicUser=` → `setgroups([])` + `setresgid`/`setresuid`
  (DynamicUser is pre-resolved host-side to the nobody uid).

Ordering (per-thread state, so `runtime.LockOSThread` then exec on that thread;
execve checks the calling thread's creds and destroys the others):

1. `PR_CAPBSET_DROP` the unwanted caps (needs `CAP_SETPCAP`, held as root).
2. **Keep-cap-as-non-root** (`KeepCaps` + non-root `User=`): add the kept caps to
   the **inheritable** set (`capset`, while still holding `CAP_SETUID`), then
   `PR_SET_KEEPCAPS` so the permitted set survives the uid change.
3. `setgroups([])`, `setresgid`, `setresuid`.
4. Raise each kept cap into the **ambient** set (`PR_CAP_AMBIENT_RAISE`) so it
   becomes effective in the exec'd non-root image.
5. `PR_SET_NO_NEW_PRIVS`.

Benefits over 0.8.1's setpriv path: works on `minimal` (no util-linux needed, so
a privilege-drop-only unit no longer forces `--profile standard`), removes the
PATH-shadowing fragility entirely (the 0.8.1 bug class), and is the prerequisite
for seccomp (must set `no_new_privs` + drop caps in the same child that installs
the filter).

Migration: cut over directly (agent and host ship together). `confine.SetprivArgs`
/ `wrapWithSetpriv` were removed; `confinementFromPlan` now carries the full spec
over vsock. `setpriv` stays baked in the `standard` profile for manual use.

Verified in-VM (`smoke-test.sh` Test 6f + ad-hoc): empty bounding set →
`CapBnd=0` + `NoNewPrivs=1` as root on `minimal`; `User=65534` +
`CapabilityBoundingSet=CAP_NET_BIND_SERVICE` → uid 65534 with `CapAmb`/`CapEff`
== that one cap; `DynamicUser` → uid 65534; ro-fs + capped bounding set composed
on `standard`. The linux-only pure logic (`capsToDrop`, `resolveDropID`, cap-name
resolution) is unit-tested in `cmd/dew-agent/caps_test.go` (CI `test-linux-agent`
job, and run in-VM against a real kernel).

---

## 5. Seccomp (`SystemCallFilter=` / `RestrictAddressFamilies=`) — DESIGN ONLY

This is the headline gap (the field notes' core unanswered question: "does the
Go runtime survive `@system-service` + W^X"). Hard parts:

1. **Group expansion.** `SystemCallFilter=@system-service` expands to systemd's
   curated set (hundreds of syscalls, arch-dependent, evolves across systemd
   versions). We must vendor a snapshot of the `@`-group → syscall mapping
   (generate it from systemd's `src/shared/seccomp-util.c` tables; pin the
   systemd version we mirror and record it). Allow/deny + the `~` (denylist)
   form both need handling, mirroring `confine`'s existing cap logic.
2. **No cgo.** `libseccomp` is cgo. Use a pure-Go BPF builder
   (`github.com/elastic/go-seccomp-bpf`, or hand-assemble with
   `golang.org/x/net/bpf` + the raw `seccomp(2)` syscall). Evaluate
   `go-seccomp-bpf`: it already does allow/deny policies and `SCMP_ACT_*`, and
   is pure-Go — likely the fastest path. Confirm it builds with
   `CGO_ENABLED=0` for linux/arm64 + linux/amd64.
3. **`RestrictAddressFamilies=`** → a seccomp arg-filter on `socket(2)`'s
   `domain` argument (allow only the listed `AF_*`). Cheap relative to (1) and
   could ship first as a standalone win.
4. **Default action & errno.** systemd uses `SCMP_ACT_ERRNO(EPERM)` (or `kill`
   under `SystemCallErrorNumber=`). Match `EPERM` so behavior mirrors prod.

Validation caveat: **cannot be verified offline** (needs a booted guest +
representative workload). Acceptance must be a boot test: a blocked syscall
returns `EPERM`; the Go runtime (threads, futex, mmap, epoll) still works under
`@system-service`.

Relationship to R1: `--confine`'s seccomp is an **approximation** built from
our mirrored group table. **R1 (systemd-as-PID1) runs the real systemd filter**
and yields the real `systemd-analyze security` score — so R1 is the
ground-truth answer and `--confine` seccomp is the fast inner loop. Ship the
RestrictAddressFamilies sub-feature first; gate full `@`-group seccomp behind
this design's acceptance.

Effort: high. Phase it: (5a) `RestrictAddressFamilies` arg-filter →
(5b) explicit named-syscall allow/deny → (5c) `@`-group expansion table.

---

## 6. R1 (systemd-as-PID1) — design exists; this is the seccomp ground truth

`docs/systemd-profile.md` already holds the R1 design (rootfs options, boot/PID1,
agent integration, `--cgroup` interaction, acceptance, build checklist). Updates
prompted by 0.8.x:

- Cross-link: R1 is what makes `systemd-analyze security <unit>` and the **real**
  `SystemCallFilter` enforcement possible; `--confine` (this doc) is the
  non-systemd approximation / fast loop. State both in each doc's intro.
- The agent-side confinement substrate (§2) is **independent** of R1 — it serves
  `--confine` on the existing Alpine profiles. R1 remains a separate rootfs/init
  track. No new coupling.
- Keep R1 opt-in and asset-gated as already designed.

No R1 implementation in this round (design-first, and it's the largest track).

---

## 7. Phasing & order

1. ✅ **Substrate** (§2): `Confinement` wire type + agent shim + `Plan →
   Confinement` mapping. Foundation for everything.
2. ✅ **Read-only fs** (§3): first behavior on the shim. In-VM tested.
3. ✅ **Native caps/uid drop** (§4): cut over from `setpriv`; removed the host
   `wrapWithSetpriv`/`SetprivArgs` path. In-VM tested.
4. **Seccomp 5a** (`RestrictAddressFamilies`): standalone, smaller. ← next
5. **Seccomp 5b/5c** (`@`-group expansion): the big one; boot-test gated.
6. **R1**: separate track, per `systemd-profile.md`.

Each step is its own atomic commit (or PR) with its own boot-test note, since
the host can't validate the guest-side mount/seccomp/prctl steps.

## 8. Risks / open questions

- **Boot-test dependency.** Steps 2–5 all need a real `standard` VM to verify;
  CI can build but not boot-assert these. Need a manual/smoke boot checklist
  per step (extend `smoke-test.sh`?).
- **`go-seccomp-bpf` no-cgo + arm64** must be confirmed before committing to it.
- **Mount-ns vs `--share`/cgroup mounts**: the read-only remount must not break
  the writable `--share :rw` dirs or the cgroup mount the agent lives in;
  enumerate and re-bind-rw them.
- **DynamicUser uid**: today resolved host-side to 65534; confirm that's the
  intended default once the agent owns the drop, or derive a per-run uid.
