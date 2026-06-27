// Package confine parses a systemd service unit's hardening directives into
// a Plan that dew approximates with kernel primitives (cgroup v2 limits +
// setpriv). It is deliberately an APPROXIMATION, not a systemd reimplementation:
// directives dew cannot enforce on a non-systemd guest are collected in
// Plan.Unsupported so the caller can warn the user rather than imply a unit
// that "passes under --confine" is guaranteed to pass under real systemd.
package confine

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Plan is the confinement derived from a unit's [Service] section.
type Plan struct {
	// cgroup v2 ceilings (0 == leave unlimited).
	MemoryBytes int64
	PidsMax     int64
	CPUQuota    int64 // cpu.max numerator for a 100000us period (200% == 200000)

	// privilege drop, rendered into a setpriv prefix by SetprivArgs.
	UID         string // numeric uid or username for setpriv --reuid
	GID         string
	DynamicUser bool     // User= absent but DynamicUser=yes → run as a fixed unprivileged uid
	DropAllCaps bool     // CapabilityBoundingSet reset to empty / positive list → start from empty
	KeepCaps    []string // with DropAllCaps: capabilities to add back (libcap names, lowercased)
	DropCaps    []string // CapabilityBoundingSet=~… : capabilities to drop from the full set
	NoNewPrivs  bool

	// Unsupported lists directives present in the unit that dew does NOT
	// enforce (seccomp, filesystem protection, address-family limits, ...).
	Unsupported []string
}

// dynamicUserUID is the uid/gid dew runs a DynamicUser= service as when the
// unit names no concrete User=. systemd allocates a transient uid; dew can't,
// so it falls back to nobody — recorded as an approximation by the parser.
const dynamicUserUID = "65534"

// Confined reports whether the plan actually constrains anything.
func (p Plan) Confined() bool {
	return p.MemoryBytes > 0 || p.PidsMax > 0 || p.CPUQuota > 0 || p.NeedsSetpriv()
}

// NeedsSetpriv reports whether the plan requires the setpriv binary in the
// guest (privilege drop) — which only the standard profile ships.
func (p Plan) NeedsSetpriv() bool {
	return p.UID != "" || p.DynamicUser || p.DropAllCaps || len(p.DropCaps) > 0 || p.NoNewPrivs
}

// SetprivArgs renders the privilege-drop prefix, e.g.
// ["setpriv","--no-new-privs","--bounding-set","-all","--reuid","65534",
// "--regid","65534","--clear-groups"]. Empty when no privilege drop applies.
func (p Plan) SetprivArgs() []string {
	if !p.NeedsSetpriv() {
		return nil
	}
	args := []string{"setpriv"}
	if p.NoNewPrivs {
		args = append(args, "--no-new-privs")
	}
	if p.DropAllCaps {
		// Positive list / empty reset: drop everything, then add back the kept
		// caps. `-all` with no `+` is a full drop.
		set := "-all"
		for _, c := range p.KeepCaps {
			set += ",+" + c
		}
		args = append(args, "--bounding-set", set)
	} else if len(p.DropCaps) > 0 {
		// Negated set (`~CAP_X`): keep the inherited full set minus these, so
		// drop only the named caps (no `-all`).
		set := ""
		for i, c := range p.DropCaps {
			if i > 0 {
				set += ","
			}
			set += "-" + c
		}
		args = append(args, "--bounding-set", set)
	}
	uid, gid := p.UID, p.GID
	if uid == "" && p.DynamicUser {
		uid = dynamicUserUID
	}
	if uid != "" {
		args = append(args, "--reuid", uid)
		if gid == "" {
			gid = uid
		}
	}
	if gid != "" {
		args = append(args, "--regid", gid, "--clear-groups")
	}
	return args
}

// ParseFile reads and parses a unit file.
func ParseFile(path string) (Plan, error) {
	f, err := os.Open(path)
	if err != nil {
		return Plan{}, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse parses a systemd unit, reading directives from its [Service] section.
func Parse(r io.Reader) (Plan, error) {
	var p Plan
	section := ""
	seenUnsupported := map[string]bool{}
	note := func(d string) {
		if !seenUnsupported[d] {
			seenUnsupported[d] = true
			p.Unsupported = append(p.Unsupported, d)
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		if section != "service" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		switch key {
		case "MemoryMax", "MemoryLimit":
			// The hard cap → cgroup memory.max.
			b, perc, err := parseBytes(val)
			if err != nil {
				return p, fmt.Errorf("%s=%q: %w", key, val, err)
			}
			if perc {
				note(key + "= (percentage of RAM not resolved)")
			} else if b > 0 {
				p.MemoryBytes = b
			}
		case "MemoryHigh":
			// A soft throttle (memory.high), not the OOM ceiling. dew applies
			// only the hard cap, so treating MemoryHigh as memory.max would
			// OOM-kill a workload that systemd would merely throttle. Surface
			// it instead of silently lowering the hard cap.
			note("MemoryHigh= (soft throttle not applied; only MemoryMax is enforced)")
		case "TasksMax":
			if n := parseCountOrInfinity(val); n > 0 {
				p.PidsMax = n
			}
		case "CPUQuota":
			if q := parseCPUQuota(val); q > 0 {
				p.CPUQuota = q
			}
		case "User":
			p.UID = val
		case "Group":
			p.GID = val
		case "DynamicUser":
			p.DynamicUser = parseBool(val)
		case "NoNewPrivileges":
			p.NoNewPrivs = parseBool(val)
		case "CapabilityBoundingSet":
			applyBoundingSet(&p, val, note)
		case "AmbientCapabilities":
			note("AmbientCapabilities= (caps are dropped, not granted, by --confine)")
		// Directives dew does not enforce — surface them, don't silently drop.
		case "SystemCallFilter", "SystemCallArchitectures":
			note(key + "= (seccomp syscall filter not applied)")
		case "RestrictAddressFamilies":
			note("RestrictAddressFamilies= (socket-family seccomp filter not applied)")
		case "ProtectSystem", "ProtectHome", "ReadOnlyPaths", "ReadWritePaths",
			"InaccessiblePaths", "PrivateTmp", "PrivateDevices", "ProtectKernelTunables",
			"ProtectKernelModules", "ProtectControlGroups", "ProtectClock",
			"ProtectKernelLogs", "ProtectHostname", "ProtectProc":
			note(key + "= (filesystem/kernel protection not applied)")
		case "MemoryDenyWriteExecute", "LockPersonality", "RestrictRealtime",
			"RestrictSUIDSGID", "RestrictNamespaces", "RemoveIPC", "PrivateUsers":
			note(key + "= (not applied)")
		}
	}
	if err := sc.Err(); err != nil {
		return p, err
	}
	if p.DynamicUser && p.UID == "" {
		note("DynamicUser=yes approximated as uid " + dynamicUserUID + " (nobody)")
	}
	return p, nil
}

// applyBoundingSet mirrors systemd's CapabilityBoundingSet semantics for the
// common cases: an empty assignment resets the set (drop all); a positive list
// resets to those caps (drop all, keep the listed); a leading "~" negates
// (keep the full set minus the listed caps).
func applyBoundingSet(p *Plan, val string, note func(string)) {
	if val == "" {
		p.DropAllCaps = true
		p.KeepCaps = nil
		return
	}
	if strings.HasPrefix(val, "~") {
		// "keep all except these" — drop only the named caps.
		for _, c := range strings.Fields(strings.TrimPrefix(val, "~")) {
			p.DropCaps = append(p.DropCaps, strings.ToLower(c))
		}
		return
	}
	p.DropAllCaps = true // start from empty, then add the listed caps back
	for _, c := range strings.Fields(val) {
		p.KeepCaps = append(p.KeepCaps, strings.ToLower(c))
	}
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "yes", "true", "1", "on":
		return true
	}
	return false
}

// parseBytes parses a systemd size (base-1024 K/M/G/T/P suffix, or bare bytes).
// Returns (bytes, isPercentage, error). "infinity" → (0,false,nil).
func parseBytes(s string) (int64, bool, error) {
	if s == "" || strings.EqualFold(s, "infinity") {
		return 0, false, nil
	}
	if strings.HasSuffix(s, "%") {
		return 0, true, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	case 'P', 'p':
		mult = 1 << 50
	}
	num := s
	if mult != 1 {
		num = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("invalid size")
	}
	if n > (1<<63-1)/mult {
		return 0, false, fmt.Errorf("size too large")
	}
	return n * mult, false, nil
}

func parseCountOrInfinity(s string) int64 {
	if strings.EqualFold(s, "infinity") {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// parseCPUQuota converts systemd's "N%" into a cpu.max quota for a 100000us
// period (100% == one core == 100000). "infinity"/"" → 0.
func parseCPUQuota(s string) int64 {
	const period = 100000
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "infinity") {
		return 0
	}
	s = strings.TrimSuffix(s, "%")
	pct, err := strconv.ParseFloat(s, 64)
	if err != nil || pct <= 0 {
		return 0
	}
	return int64(pct / 100 * period)
}
