//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// Read-only filesystem first — it needs the mount namespace and root.
	if c.ReadOnlyRoot {
		if err := applyReadOnlyFS(&c); err != nil {
			return err
		}
	}
	// Native privilege drop (capability bounding set, ambient caps, no_new_privs,
	// uid/gid) — replaces the host-built setpriv prefix. Also honours
	// DEW_EXEC_USER (unprivileged-exec mode) when the unit names no User=, so a
	// confinement never runs the target as root when the unconfined path would
	// have dropped it.
	if err := applyPrivilegeDrop(&c, os.Getenv("DEW_EXEC_USER")); err != nil {
		return err
	}
	path, err := exec.LookPath(target[0])
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target[0], err)
	}
	// Seccomp filters last — after LookPath so an allowlist doesn't EPERM the
	// PATH resolution, and on the same thread applyPrivilegeDrop locked (which
	// already set no_new_privs for us). The only syscall after this is the execve
	// below; the env cleanup in between is in-memory.
	if err := applySeccomp(&c); err != nil {
		return err
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
	// Materialize missing writable-exception paths while the tree is still
	// writable (the state-directory case, e.g. /var/lib/app). A MISSING entry is
	// always created as a directory: file-vs-directory intent can't be inferred
	// from a non-existent path (both /var/lib/app and /etc/resolv.conf are given
	// without a trailing slash), and the directory default is what state dirs
	// need. Consequence: a FILE exception must already exist to be bound as a
	// file (dew binds an existing file path directly — a dew refinement over
	// systemd, which routes a non-directory entry to its ancestor directory). An
	// existing entry (file or dir) is left as-is; bindReadWrite binds it by type.
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
// recursive bind of a file is invalid. Binding an existing file path directly
// (e.g. ReadWritePaths=/etc/hostname) is a dew refinement; systemd instead
// routes a non-directory entry to its ancestor directory. A missing entry was
// created as a directory by applyReadOnlyFS, so it binds as a directory here.
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
	spec, err := json.Marshal(req.Confine)
	if err != nil {
		// Fail closed: hand the shim a non-empty, unparseable spec so it errors
		// out (json.Unmarshal fails) instead of running the target unconfined.
		// An empty value would be read as "no confinement" — a silent bypass.
		spec = []byte("dew: confine spec marshal failed")
	}
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
