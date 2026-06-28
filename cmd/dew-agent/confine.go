//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/solcreek/dew/internal/guestenv"
	protocol "github.com/solcreek/dew/internal/vsock"
	"golang.org/x/sys/unix"
)

// confineShimMarker is the argv[1] sentinel that turns a re-exec of the agent
// binary into the confinement shim. Layout: [exe, marker, targetCmd, args...],
// with the Confinement spec passed in DEW_CONFINE_SPEC.
const confineShimMarker = "--dew-confine-shim"

// maybeRunConfineShim runs the confinement shim and never returns when the
// process was launched as the shim — it sets up the namespace/filesystem, then
// exec()s the target (or exits non-zero). Call it at the top of main(), AFTER
// the PATH pin so the shim's LookPath resolves binaries the same way.
//
// Why a re-exec shim: Go can't run arbitrary code in the child between fork and
// exec, but read-only-fs needs imperative mount(2) calls inside the new mount
// namespace before the target starts. The parent clones with CLONE_NEWNS
// (confineCloneFlags); this shim, already inside that namespace, does the work.
func maybeRunConfineShim() {
	if len(os.Args) < 3 || os.Args[1] != confineShimMarker {
		return
	}
	if err := runConfineShim(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "dew-confine: %v\n", err)
		os.Exit(126)
	}
}

func runConfineShim(target []string) error {
	if len(target) == 0 {
		return fmt.Errorf("no target command")
	}
	var c protocol.Confinement
	if s := os.Getenv("DEW_CONFINE_SPEC"); s != "" {
		if err := json.Unmarshal([]byte(s), &c); err != nil {
			return fmt.Errorf("bad confine spec: %w", err)
		}
	}
	// Phase: only the read-only filesystem is applied here; the capability/uid
	// drop still rides the host-built setpriv prefix in `target`. Native caps
	// drop (PR_CAPBSET_DROP/setresuid) lands in a follow-up.
	if c.ReadOnlyRoot {
		if err := applyReadOnlyFS(&c); err != nil {
			return err
		}
	}
	// Preserve unprivileged-exec mode. The unconfined path drops to
	// DEW_EXEC_USER (setExecUser); the shim must too, or a confinement that
	// doesn't itself drop the uid would run the target as root — an escalation
	// relative to the unconfined path. Done here, after the mounts (which need
	// root). Skipped only when the setpriv prefix actually performs an ID drop
	// (--reuid/--regid): there the unit's own User= takes over and setpriv
	// needs root to do it, so we must not drop first. A caps/no_new_privs-only
	// setpriv does NOT drop uid, so we still drop to DEW_EXEC_USER (setpriv can
	// then drop caps/nnp as the unprivileged user).
	if u := os.Getenv("DEW_EXEC_USER"); u != "" && !setprivDropsID(target) {
		if err := dropToUser(u); err != nil {
			return fmt.Errorf("drop to user %q: %w", u, err)
		}
	}
	path, err := exec.LookPath(target[0])
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target[0], err)
	}
	// Don't leak the shim's internal control channel into the confined process.
	os.Unsetenv("DEW_CONFINE_SPEC")
	return syscall.Exec(path, target, os.Environ())
}

// applyReadOnlyFS remounts the root filesystem read-only inside the (already
// unshared) mount namespace, keeping API filesystems and the declared
// ReadWritePaths writable. Mirrors systemd ProtectSystem=strict.
func applyReadOnlyFS(c *protocol.Confinement) error {
	// Reject relative paths: Stat/MkdirAll/mount on a relative entry would act
	// relative to the cwd (req.Dir), mounting outside the intended location.
	// systemd ReadWritePaths are always absolute.
	for _, p := range c.ReadWritePaths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("read-write path %q is not absolute", p)
		}
	}
	// Make every mount in this namespace private so our remounts don't
	// propagate back to the agent's / host's view.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / rprivate: %w", err)
	}
	// Materialize missing writable-exception dirs while the tree is still
	// writable (systemd likewise creates ReadWritePaths before protecting the
	// fs). Existing entries are left as-is: MkdirAll on an existing file would
	// fail, and bindReadWrite handles files and dirs alike. Note dew binds a
	// file path (e.g. /etc/resolv.conf) directly — a dew refinement; systemd
	// instead handles non-directory paths via their closest ancestor directory.
	for _, p := range c.ReadWritePaths {
		if _, err := os.Stat(p); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat read-write path %q: %w", p, err)
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("create read-write path %q: %w", p, err)
		}
	}
	// Recursively bind / onto itself first. A per-mount read-only remount
	// (MS_BIND|MS_REMOUNT) needs a bind mount to operate on, and this is the
	// proven container-runtime idiom; it also keeps the change per-mount rather
	// than touching the shared superblock (a plain MS_REMOUNT|MS_RDONLY would
	// flip the root filesystem read-only for the host/agent too, since
	// superblock flags aren't namespaced). MS_REC so submounts are preserved.
	if err := unix.Mount("/", "/", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mount / : %w", err)
	}
	// Flip only the root mount read-only — non-recursive, so submounts like
	// /proc, /sys, /dev, /tmp, /run, the cgroup mount and virtiofs --share dirs
	// keep their own (writable) flags, exactly as systemd leaves API
	// filesystems writable under ProtectSystem=strict.
	if err := unix.Mount("", "/", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount / read-only: %w", err)
	}
	for _, p := range c.ReadWritePaths {
		if err := bindReadWrite(p); err != nil {
			return fmt.Errorf("read-write path %q: %w", p, err)
		}
	}
	return nil
}

// bindReadWrite restores write access to p over the read-only root by binding
// it onto itself and clearing MS_RDONLY. MS_REC only for directories — a
// recursive bind of a file exception (e.g. ReadWritePaths=/etc/resolv.conf) is
// invalid. dew binds a file path directly; systemd instead handles
// non-directory paths via their closest ancestor directory.
func bindReadWrite(p string) error {
	flags := uintptr(unix.MS_BIND)
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		flags |= unix.MS_REC
	}
	if err := unix.Mount(p, p, "", flags, ""); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	if err := unix.Mount("", p, "", unix.MS_BIND|unix.MS_REMOUNT, ""); err != nil {
		return fmt.Errorf("remount rw: %w", err)
	}
	return nil
}

// setprivDropsID reports whether target is a setpriv invocation that changes
// the uid/gid (--reuid/--regid). The host emits these as separate argv tokens
// (see confine.SetprivArgs), so an exact match is sufficient. Only such an
// invocation makes the shim's own DEW_EXEC_USER drop redundant; a setpriv that
// only drops caps/no_new_privs leaves the uid as root.
func setprivDropsID(target []string) bool {
	if len(target) == 0 || filepath.Base(target[0]) != "setpriv" {
		return false
	}
	for _, a := range target[1:] {
		if a == "--reuid" || a == "--regid" {
			return true
		}
		if a == "--" {
			break // end of setpriv options; the rest is the wrapped command
		}
	}
	return false
}

// dropToUser drops gid/groups/uid to the named user (DEW_EXEC_USER) before
// exec, mirroring setExecUser on the unconfined path. groups then gid then uid,
// so each step is still permitted while we hold root.
//
// Supplementary groups are CLEARED, matching the unconfined path: Go runs the
// DEW_EXEC_USER exec via SysProcAttr.Credential{Uid,Gid} with no Groups, which
// calls setgroups to empty. Keeping confined == unconfined here is the point;
// we deliberately do not initgroups the user's full group list (that would be
// more permissive than the unconfined path — systemd User= supplementary-group
// fidelity is the setpriv path's job, not this DEW_EXEC_USER mirror).
func dropToUser(name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("gid %q: %w", u.Gid, err)
	}
	if err := unix.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := unix.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	if err := unix.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	return nil
}

// confineCloneFlags returns the namespace clone flags the parent needs so the
// shim can apply the spec (a mount namespace for the read-only fs).
func confineCloneFlags(c *protocol.Confinement) uintptr {
	var f uintptr
	if c != nil && c.ReadOnlyRoot {
		f |= unix.CLONE_NEWNS
	}
	return f
}

// confineExecCmd builds the re-exec into the shim for a confined batch exec,
// carrying the spec in the environment and requesting the needed namespaces.
func confineExecCmd(ctx context.Context, req protocol.ExecRequest) *exec.Cmd {
	spec, _ := json.Marshal(req.Confine)
	argv := append([]string{confineShimMarker, req.Command}, req.Args...)
	cmd := exec.CommandContext(ctx, "/proc/self/exe", argv...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	// Strip any caller-supplied DEW_CONFINE_SPEC before appending the
	// authoritative one. req.Env entries sort before our append, and the shim's
	// os.Getenv returns the first match — so without this a workload could
	// inject its own spec via req.Env and weaken or break the confinement.
	env := guestenv.ExecEnv(os.Environ(), req.Env)
	cleaned := env[:0:0]
	for _, e := range env {
		if strings.HasPrefix(e, "DEW_CONFINE_SPEC=") {
			continue
		}
		cleaned = append(cleaned, e)
	}
	cmd.Env = append(cleaned, "DEW_CONFINE_SPEC="+string(spec))
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: confineCloneFlags(req.Confine)}
	return cmd
}
