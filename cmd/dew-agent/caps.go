//go:build linux

package main

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	protocol "github.com/solcreek/dew/internal/vsock"
	"golang.org/x/sys/unix"
)

// lookupUser resolves a User= spec (numeric uid or username) to its uid and
// primary gid. A numeric uid that isn't in /etc/passwd falls back to gid==uid.
func lookupUser(spec string) (uid, gid int, err error) {
	if n, e := strconv.Atoi(spec); e == nil {
		if n < 0 {
			// setresuid(-1) means "leave unchanged" — a negative uid would
			// silently skip the drop, so reject it rather than under-confine.
			return 0, 0, fmt.Errorf("negative uid %d", n)
		}
		uid = n
		gid = n
		if u, e := user.LookupId(spec); e == nil {
			if g, e := strconv.Atoi(u.Gid); e == nil {
				gid = g
			}
		}
		return uid, gid, nil
	}
	u, e := user.Lookup(spec)
	if e != nil {
		return 0, 0, fmt.Errorf("user %q: %w", spec, e)
	}
	uid, err = strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("uid %q: %w", u.Uid, err)
	}
	gid, err = strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("gid %q: %w", u.Gid, err)
	}
	return uid, gid, nil
}

// lookupGroup resolves a Group= spec (numeric gid or group name) to its gid.
func lookupGroup(spec string) (int, error) {
	if n, e := strconv.Atoi(spec); e == nil {
		if n < 0 {
			return 0, fmt.Errorf("negative gid %d", n)
		}
		return n, nil
	}
	g, e := user.LookupGroup(spec)
	if e != nil {
		return 0, fmt.Errorf("group %q: %w", spec, e)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("gid %q: %w", g.Gid, err)
	}
	return gid, nil
}

// capByName maps the lowercase libcap names that confine.Plan emits
// (CapabilityBoundingSet= tokens) to their stable kernel capability numbers.
// The numbers are ABI-stable, so we key off them directly rather than the
// golang.org/x/sys/unix CAP_* constants (which may lag a new kernel cap).
var capByName = map[string]int{
	"cap_chown":              0,
	"cap_dac_override":       1,
	"cap_dac_read_search":    2,
	"cap_fowner":             3,
	"cap_fsetid":             4,
	"cap_kill":               5,
	"cap_setgid":             6,
	"cap_setuid":             7,
	"cap_setpcap":            8,
	"cap_linux_immutable":    9,
	"cap_net_bind_service":   10,
	"cap_net_broadcast":      11,
	"cap_net_admin":          12,
	"cap_net_raw":            13,
	"cap_ipc_lock":           14,
	"cap_ipc_owner":          15,
	"cap_sys_module":         16,
	"cap_sys_rawio":          17,
	"cap_sys_chroot":         18,
	"cap_sys_ptrace":         19,
	"cap_sys_pacct":          20,
	"cap_sys_admin":          21,
	"cap_sys_boot":           22,
	"cap_sys_nice":           23,
	"cap_sys_resource":       24,
	"cap_sys_time":           25,
	"cap_sys_tty_config":     26,
	"cap_mknod":              27,
	"cap_lease":              28,
	"cap_audit_write":        29,
	"cap_audit_control":      30,
	"cap_setfcap":            31,
	"cap_mac_override":       32,
	"cap_mac_admin":          33,
	"cap_syslog":             34,
	"cap_wake_alarm":         35,
	"cap_block_suspend":      36,
	"cap_audit_read":         37,
	"cap_perfmon":            38,
	"cap_bpf":                39,
	"cap_checkpoint_restore": 40,
}

// resolveCap maps a libcap name to its capability number.
func resolveCap(name string) (int, error) {
	if n, ok := capByName[strings.ToLower(name)]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("unknown capability %q", name)
}

// lastCapability returns the highest capability the running kernel knows about,
// so the bounding-set sweep doesn't PR_CAPBSET_DROP a number the kernel rejects
// with EINVAL. Reads /proc/sys/kernel/cap_last_cap; falls back to the build-time
// constant.
func lastCapability() int {
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			return n
		}
	}
	return unix.CAP_LAST_CAP
}

// capsToDrop returns the capability numbers to remove from the bounding set for
// the given spec, bounded by lastCap. DropAllCaps → every cap except KeepCaps;
// DropCaps → only the named caps. Pure (no syscalls) so it is unit-testable.
func capsToDrop(c *protocol.Confinement, lastCap int) ([]int, error) {
	if c.DropAllCaps {
		keep := map[int]bool{}
		for _, n := range c.KeepCaps {
			id, err := resolveCap(n)
			if err != nil {
				return nil, err
			}
			keep[id] = true
		}
		var drop []int
		for i := 0; i <= lastCap; i++ {
			if !keep[i] {
				drop = append(drop, i)
			}
		}
		return drop, nil
	}
	var drop []int
	for _, n := range c.DropCaps {
		id, err := resolveCap(n)
		if err != nil {
			return nil, err
		}
		// A cap the running kernel doesn't have isn't in the bounding set, so
		// dropping it is a no-op; skip it to avoid PR_CAPBSET_DROP EINVAL.
		if id > lastCap {
			continue
		}
		drop = append(drop, id)
	}
	return drop, nil
}

// keptCaps resolves the KeepCaps names to numbers (only meaningful with
// DropAllCaps), skipping any the running kernel lacks (id > lastCap) — those
// can't be inherited or ambient-raised (capset / PR_CAP_AMBIENT_RAISE would
// EINVAL) and aren't in the bounding set anyway. Pure/testable.
func keptCaps(c *protocol.Confinement, lastCap int) ([]int, error) {
	if !c.DropAllCaps {
		return nil, nil
	}
	var keep []int
	for _, n := range c.KeepCaps {
		id, err := resolveCap(n)
		if err != nil {
			return nil, err
		}
		if id > lastCap {
			continue
		}
		keep = append(keep, id)
	}
	return keep, nil
}

// dynamicUserUID is the fallback uid for DynamicUser=yes when the unit names no
// concrete User=. The host normally pre-resolves this (confinementFromPlan), so
// this is defence-in-depth; it must match confine.DynamicUserUID (nobody).
const dynamicUserUID = "65534"

// idSpec is the resolved identity to drop to. uid and gid are independent: a
// Group=-only unit drops the gid (and clears supplementary groups) while leaving
// the uid as root, mirroring the old setpriv `--regid --clear-groups` with no
// `--reuid`.
type idSpec struct {
	uid, gid       int
	setUID, setGID bool
}

// resolveDropID decides the final identity. A user (the unit's User=, else
// DynamicUser's fixed uid, else DEW_EXEC_USER) drops both uid and gid (gid to
// the user's primary group). Group= drops/overrides only the gid. When neither
// applies, nothing is set and the target keeps the current (root) identity.
// Pure except for /etc/passwd lookups of names.
func resolveDropID(c *protocol.Confinement, execUser string) (idSpec, error) {
	var id idSpec
	name := c.User
	if name == "" && c.DynamicUser {
		name = dynamicUserUID
	}
	if name == "" {
		name = execUser
	}
	if name != "" {
		uid, primaryGID, err := lookupUser(name)
		if err != nil {
			return idSpec{}, err
		}
		id.uid, id.setUID = uid, true
		id.gid, id.setGID = primaryGID, true
	}
	if c.Group != "" {
		g, err := lookupGroup(c.Group)
		if err != nil {
			return idSpec{}, err
		}
		id.gid, id.setGID = g, true
	}
	return id, nil
}

// applyPrivilegeDrop performs the native uid/gid/capability/no_new_privs drop in
// the shim child, replacing the host-side setpriv prefix. It runs after the
// mount-namespace work (which needs root) and just before exec. It also sets
// no_new_privs when a seccomp spec is present (seccomp(2) needs it); the seccomp
// filter itself is installed later, by runConfineShim after LookPath, on the
// thread this locks.
//
// Capabilities, no_new_privs and credentials are per-thread (per-task) in Linux,
// and execve checks the calling thread's credentials and destroys the others.
// We therefore LockOSThread for the whole sequence so the same thread that drops
// privilege is the one that execs; the agent's other (still-root) threads vanish
// at execve and never run target code. The shim has no other goroutines doing
// work here, so there is no exploitable window.
//
// Order matters. Bounding set first (needs CAP_SETPCAP, held as root). Then, for
// the keep-caps-as-non-root case, set the inheritable set while we still hold
// CAP_SETUID, flag PR_SET_KEEPCAPS so the permitted set survives the uid change,
// drop gid→uid, raise the kept caps into the ambient set (so they become
// effective after execve), and finally set no_new_privs.
func applyPrivilegeDrop(c *protocol.Confinement, execUser string) error {
	id, err := resolveDropID(c, execUser)
	if err != nil {
		return err
	}
	lastCap := lastCapability()
	drop, err := capsToDrop(c, lastCap)
	if err != nil {
		return err
	}
	keep, err := keptCaps(c, lastCap)
	if err != nil {
		return err
	}
	// seccomp (RestrictAddressFamilies=) needs no_new_privs and must be installed
	// on the same thread that execs, so it rides this sequence.
	wantSeccomp := needsSeccomp(c)

	// Nothing to enforce → leave the process (and its thread) untouched.
	if !id.setUID && !id.setGID && len(drop) == 0 && !c.NoNewPrivs && !wantSeccomp {
		return nil
	}

	runtime.LockOSThread()

	// Bounding set: cap the ceiling. For a root target this is what limits the
	// post-exec capabilities; for a non-root target the uid drop clears them too.
	for _, capn := range drop {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capn), 0, 0, 0); err != nil {
			return fmt.Errorf("drop bounding cap %d: %w", capn, err)
		}
	}

	// Keep specific caps across a drop to a non-root uid: needs the ambient set.
	// (For a root target, the bounding set already preserves them across execve.)
	wantAmbient := id.setUID && id.uid != 0 && len(keep) > 0
	if wantAmbient {
		// Add the kept caps to the inheritable set while we still hold full
		// permitted/effective (so CAP_SETUID survives for the drop below).
		if err := addInheritable(keep); err != nil {
			return fmt.Errorf("set inheritable caps: %w", err)
		}
		// Retain the permitted set across the uid transition.
		if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("set keepcaps: %w", err)
		}
	}

	// Clear supplementary groups whenever we touch the identity (matches the old
	// setpriv --clear-groups), then gid before uid so each step is still
	// permitted while we hold root.
	if id.setGID || id.setUID {
		if err := unix.Setgroups([]int{}); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
	}
	if id.setGID {
		if err := unix.Setresgid(id.gid, id.gid, id.gid); err != nil {
			return fmt.Errorf("setresgid: %w", err)
		}
	}
	if id.setUID {
		if err := unix.Setresuid(id.uid, id.uid, id.uid); err != nil {
			return fmt.Errorf("setresuid: %w", err)
		}
	}

	if wantAmbient {
		// permitted (kept via KEEPCAPS) + inheritable (set above) → ambient raise
		// is permitted; the kept caps become effective in the exec'd image.
		for _, capn := range keep {
			if err := unix.Prctl(unix.PR_CAP_AMBIENT, uintptr(unix.PR_CAP_AMBIENT_RAISE), uintptr(capn), 0, 0); err != nil {
				return fmt.Errorf("raise ambient cap %d: %w", capn, err)
			}
		}
	}

	// no_new_privs last: it never blocks raising already-held caps, and seccomp(2)
	// requires it (or CAP_SYS_ADMIN) for an unprivileged caller. A seccomp spec
	// implies no_new_privs, mirroring systemd. The filter itself is installed by
	// runConfineShim after LookPath (so an allowlist doesn't EPERM the PATH
	// resolution), on this same locked thread.
	if c.NoNewPrivs || wantSeccomp {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("set no_new_privs: %w", err)
		}
	}
	return nil
}

// addInheritable adds the given capabilities to the current thread's inheritable
// set, preserving the permitted/effective sets. A thread may add to inheritable
// any capability already in its permitted set, so as root (full permitted) this
// succeeds for any cap still within the bounding set.
func addInheritable(caps []int) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3, Pid: 0}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return err
	}
	for _, c := range caps {
		data[c>>5].Inheritable |= 1 << uint(c&31)
	}
	return unix.Capset(&hdr, &data[0])
}
