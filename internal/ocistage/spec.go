package ocistage

import (
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// parseUser maps an image config User ("", "1000", "1000:1000", or a name) to
// a numeric uid/gid for the OCI spec. Numeric forms are honored; an empty or
// non-numeric (name-based) User falls back to root, since the guest launcher
// does not resolve /etc/passwd. When only a uid is given, gid mirrors it (the
// common "USER 1000" convention).
func parseUser(u string) (uid, gid int) {
	if u == "" {
		return 0, 0
	}
	parts := strings.SplitN(u, ":", 2)
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0
	}
	uid, gid = n, n
	if len(parts) == 2 {
		if g, gerr := strconv.Atoi(parts[1]); gerr == nil {
			gid = g
		}
	}
	return uid, gid
}

// Bind is an optional host→container bind mount used for service data
// persistence: Source is a path on the guest's ext4 disk, Destination is the
// in-container path (e.g. /var/lib/postgresql/data).
type Bind struct {
	Source      string
	Destination string
}

// ociSpec builds a minimal-but-valid OCI runtime spec from the image config.
//
// Host networking semantics: no "network" namespace, so the container shares
// the VM's network (matches dew's --net=host service model). The VM is the
// isolation boundary, so we grant a broad capability set rather than the
// restrictive runc default — postgres/mysql entrypoints drop privileges and
// need SETUID/SETGID/CHOWN/DAC_OVERRIDE/FOWNER to do so.
//
// rootPath is the absolute path to the overlay merged dir the guest launcher
// sets up. cmdOverride replaces the image entrypoint+cmd when non-empty;
// extraEnv is appended to the image env. bind, when non-nil, adds a rw bind
// mount for persistent data.
func ociSpec(c v1.Config, rootPath string, cmdOverride, extraEnv []string, bind *Bind) map[string]any {
	args := cmdOverride
	if len(args) == 0 {
		args = append(append([]string{}, c.Entrypoint...), c.Cmd...)
	}
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}

	env := append([]string{}, c.Env...)
	if len(env) == 0 {
		env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	env = append(env, extraEnv...)

	cwd := c.WorkingDir
	if cwd == "" {
		cwd = "/"
	}

	uid, gid := parseUser(c.User)

	caps := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER",
		"CAP_MKNOD", "CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP",
		"CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
	}
	capSet := map[string]any{"bounding": caps, "effective": caps, "permitted": caps}

	mount := func(dst, typ, src string, opts ...string) map[string]any {
		return map[string]any{"destination": dst, "type": typ, "source": src, "options": opts}
	}
	mounts := []map[string]any{
		mount("/proc", "proc", "proc"),
		mount("/dev", "tmpfs", "tmpfs", "nosuid", "strictatime", "mode=755", "size=65536k"),
		mount("/dev/pts", "devpts", "devpts", "nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"),
		mount("/dev/shm", "tmpfs", "shm", "nosuid", "noexec", "nodev", "mode=1777", "size=65536k"),
		mount("/dev/mqueue", "mqueue", "mqueue", "nosuid", "noexec", "nodev"),
		mount("/sys", "sysfs", "sysfs", "nosuid", "noexec", "nodev", "ro"),
	}
	if bind != nil {
		mounts = append(mounts, mount(bind.Destination, "bind", bind.Source, "rbind", "rw"))
	}

	return map[string]any{
		"ociVersion": "1.0.2-dev",
		"process": map[string]any{
			"terminal":        false,
			"user":            map[string]any{"uid": uid, "gid": gid},
			"args":            args,
			"env":             env,
			"cwd":             cwd,
			"noNewPrivileges": true,
			"capabilities":    capSet,
		},
		"root": map[string]any{
			"path":     rootPath,
			"readonly": false,
		},
		"hostname": "dew-oci",
		"mounts":   mounts,
		"linux": map[string]any{
			"namespaces": []map[string]any{
				{"type": "pid"},
				{"type": "ipc"},
				{"type": "uts"},
				{"type": "mount"},
				// no "network" -> share VM network (host networking)
			},
			"maskedPaths": []string{
				"/proc/kcore", "/proc/latency_stats", "/proc/timer_list",
				"/proc/timer_stats", "/proc/sched_debug", "/sys/firmware",
			},
			"readonlyPaths": []string{
				"/proc/asound", "/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
			},
		},
	}
}
