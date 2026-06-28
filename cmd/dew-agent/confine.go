//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	path, err := exec.LookPath(target[0])
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target[0], err)
	}
	return syscall.Exec(path, target, os.Environ())
}

// applyReadOnlyFS remounts the root filesystem read-only inside the (already
// unshared) mount namespace, keeping API filesystems and the declared
// ReadWritePaths writable. Mirrors systemd ProtectSystem=strict.
func applyReadOnlyFS(c *protocol.Confinement) error {
	// Make every mount in this namespace private so our remounts don't
	// propagate back to the agent's / host's view.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / rprivate: %w", err)
	}
	// Create the writable exception dirs while the tree is still writable
	// (systemd likewise materializes ReadWritePaths before protecting the fs).
	for _, p := range c.ReadWritePaths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("create read-write path %q: %w", p, err)
		}
	}
	// Flip the root mount read-only. Non-recursive on purpose: submounts like
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
// it onto itself and clearing MS_RDONLY.
func bindReadWrite(p string) error {
	if err := unix.Mount(p, p, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
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
	spec, _ := json.Marshal(req.Confine)
	argv := append([]string{confineShimMarker, req.Command}, req.Args...)
	cmd := exec.CommandContext(ctx, "/proc/self/exe", argv...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	cmd.Env = append(guestenv.ExecEnv(os.Environ(), req.Env), "DEW_CONFINE_SPEC="+string(spec))
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: confineCloneFlags(req.Confine)}
	return cmd
}
