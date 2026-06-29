# Design: `systemd` profile (R1)

Status: **rootfs builder landed; host wiring + asset release pending.**
`initramfs/build-systemd.sh` produces a bootable Debian + systemd cpio that, under
dew's Alpine `linux-virt` kernel, boots with PID 1 = systemd, dew-agent supervised
over vsock, and `systemctl start` / `systemd-analyze security` working end to end
(validated 2026-06-29 on the colima/lima build VM). `dew run --profile systemd` /
`dew vm start --profile systemd` still exit `105` (`unavailable`) until the host
wiring and asset release land — see the build-surface checklist below.

## Why a separate profile (and why it's opt-in)

The deployment artifact people most want to test locally is a **systemd
`.service` unit** — `systemctl start`, then `systemd-analyze security <unit>`
for the exposure score, plus the systemd-specific semantics (`DynamicUser`,
`ProtectSystem=strict`, `ReadWritePaths`, `SystemCallFilter=@system-service`
group expansion) that `--confine` only *approximates* (see
[`internal/confine`](../internal/confine/confine.go)).

dew's existing profiles are **Alpine** (busybox + OpenRC-less custom init, PID 1
is `/bin/sh`). Alpine ships **no systemd** in any repo, so this profile cannot
reuse the Alpine pipeline — it needs a systemd-based distro rootfs. That makes
it heavier (bigger image, slower boot) and against the lightweight grain of
`minimal`/`node`, so it must stay **opt-in and never a `detect.Detect()`
default**. The lightweight profiles are unchanged.

## Rootfs options (pick one during implementation)

| Base | Pros | Cons |
|---|---|---|
| **Debian (debootstrap `--variant=minbase` + `systemd`)** | reproducible, well-documented, small-ish (~120–180MB), matches most users' prod | needs a Debian build host or container; second package pipeline (`apt`) alongside `apk` |
| Ubuntu cloud rootfs tarball | prebuilt, fast to fetch | larger; snapd/cloud-init cruft to strip |
| Fedora (dnf `--installroot`) | newest systemd, good `systemd-analyze` | largest; rpm pipeline |

**Recommendation: Debian minbase + systemd**, built in a Debian container so the
host stays apk-only. Keep it on the disk-bearing path (like `standard`): systemd
+ journald want a real writable filesystem, and `switch_root` to ext4 is already
implemented.

## Boot & PID 1

- Kernel: reuse the shipped Alpine `linux-virt` kernel + the same module set
  (modules come from dew's initramfs, not the rootfs, so the Debian userland is
  fine). systemd's hard requirements are already satisfied by the module
  allowlist or must be added to it: **cgroup v2** (present), `autofs4`
  (systemd's `RequiresMountsFor`/automount — add if missing), `binfmt_misc`
  (already added for Rosetta). Verify `CONFIG_*` the journal needs.
- cgroup v2 unified hierarchy only (systemd ≥ v248 defaults to it; we already
  mount `cgroup2` at `/sys/fs/cgroup`). Ensure dew's init does **not**
  pre-create `/sys/fs/cgroup/dew` or enable `subtree_control` on this profile —
  systemd owns the hierarchy and rejects a populated root. The R4 cgroup block
  must be gated off when PID 1 is systemd.
- PID 1: `init=/sbin/init` (systemd) on the kernel cmdline; dew's two-stage
  `/init` → `/init-stage2` still runs first to do early mount / disk / network
  bring-up, then `exec /sbin/init` instead of `exec /bin/sh`.

## dew-agent integration (the critical part)

vsock exec (`dew exec`, port forwarding, token handshake) must keep working when
systemd owns PID 1. Today `init-stage2` backgrounds `dew-agent` directly.

Plan: ship a unit `dew-agent.service` and let systemd supervise it.

```ini
[Unit]
Description=dew guest agent
DefaultDependencies=no
After=local-fs.target systemd-sysctl.service
Before=sysinit.target

[Service]
Type=simple
ExecStart=/usr/local/bin/dew-agent
Restart=on-failure
# token still arrives over vsock SetTokenRequest; never on cmdline/disk

[Install]
WantedBy=sysinit.target
```

- Stage `dew.cmd=` (the boot-time command) handling: for systemd, drop it onto a
  oneshot unit or just rely on the vsock exec path (`dew run` already delivers
  over vsock, not cmdline).
- Networking: hand off to `systemd-networkd` (simple DHCP `.network`) instead of
  dew's busybox `udhcpc`, OR keep dew's early DHCP in `/init-stage2` before
  `exec /sbin/init` and mask `systemd-networkd`. Prefer the latter for speed.

## Interaction with R4 `--cgroup`

`--cgroup` and `--confine`'s cgroup limits assume dew owns `/sys/fs/cgroup/dew`.
Under systemd, apply caps the systemd way instead: drop a slice/scope drop-in
(`MemoryMax=`/`TasksMax=`/`CPUQuota=`) rather than writing `cgroup.*` files. The
host already parses the values (R4); only the guest application differs. Until
that's wired, `--cgroup` should be rejected together with `--profile systemd`.

## Acceptance criteria

```sh
dew run --profile systemd --share ./deploy:ro -- sh -c '
  cp /deploy/x.service /etc/systemd/system/ &&
  systemctl daemon-reload &&
  systemctl start x &&
  systemctl is-active x &&
  systemd-analyze security x'
```

1. PID 1 is `systemd` (`cat /proc/1/comm` → `systemd`).
2. `systemctl start <unit>` runs the unit; `systemctl is-active` reports it.
3. `systemd-analyze security <unit>` returns an exposure score.
4. `DynamicUser=`, `ProtectSystem=strict`, `SystemCallFilter=` actually take
   effect (a blocked syscall returns `EPERM`; a write to a protected path
   fails).
5. `dew exec` into the running VM still works (agent supervised by systemd).
6. Boot time and image size recorded; documented as the heavy tier.

## Build surface (follow-up checklist)

- [x] `initramfs/build-systemd.sh` (sibling script) producing a Debian rootfs +
      systemd, on a Debian/Ubuntu root host (the colima/lima VM or a CI
      container); injects dew's matching kernel modules from an existing
      initramfs and reuses `vmlinuz-<arch>`. Outputs `initramfs-systemd-<arch>.cpio.gz`.
- [ ] Module allowlist review for systemd (autofs, etc.). (`virtio_console` is
      built-in, so `console=hvc0` works at early boot; vsock/virtio/ext4 are
      loaded via `/etc/modules-load.d/dew.conf`.)
- [ ] CI: install `debian-archive-keyring` so debootstrap verifies signatures
      (the local Ubuntu build only warns).
- [ ] Two-stage init: the current builder boots systemd directly from the cpio
      (tmpfs rootfs — fine for dew's ephemeral model). For journald persistence,
      add the `/init-stage2` branch that `switch_root`s to ext4 then
      `exec /sbin/init`, and skip the R4 cgroup block when PID 1 is systemd.
- [x] `dew-agent.service` baked + enabled (`build-systemd.sh` installs it and
      symlinks it under `sysinit.target.wants`).
- [ ] `applyProfileDefaults` entry (RAM/disk for the heavy tier).
- [ ] Remove the `CodeUnavailable` guard in `parseFlags` once assets exist.
- [ ] Gate/translate `--cgroup` + `--confine` to systemd drop-ins under this
      profile.
- [ ] CI: a Linux runner that boots the profile and runs the acceptance script.
