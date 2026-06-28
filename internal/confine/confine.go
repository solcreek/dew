// Package confine parses a systemd service unit's hardening directives into
// a Plan that dew approximates with kernel primitives (cgroup v2 limits, a
// native prctl/capset uid/caps drop, a read-only mount namespace, and a
// socket-family seccomp filter). It is deliberately an APPROXIMATION, not a
// systemd reimplementation:
// directives dew cannot enforce on a non-systemd guest are collected in
// Plan.Unsupported so the caller can warn the user rather than imply a unit
// that "passes under --confine" is guaranteed to pass under real systemd.
package confine

//go:generate go run gen_syscallgroups.go

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Plan is the confinement derived from a unit's [Service] section.
type Plan struct {
	// cgroup v2 ceilings (0 == leave unlimited).
	MemoryBytes int64
	PidsMax     int64
	CPUQuota    int64 // cpu.max numerator for a 100000us period (200% == 200000)

	// privilege drop, applied natively by the agent shim (prctl/capset/setresuid).
	UID         string // numeric uid or username to drop to
	GID         string
	DynamicUser bool     // User= absent but DynamicUser=yes → run as a fixed unprivileged uid
	DropAllCaps bool     // CapabilityBoundingSet reset to empty / positive list → start from empty
	KeepCaps    []string // with DropAllCaps: capabilities to add back (libcap names, lowercased)
	DropCaps    []string // CapabilityBoundingSet=~… : capabilities to drop from the full set
	NoNewPrivs  bool

	// filesystem isolation (ProtectSystem=strict + ReadWritePaths=), applied
	// by the agent in a mount namespace before exec.
	ReadOnlyRoot   bool     // ProtectSystem=strict → remount the rootfs read-only
	ReadWritePaths []string // paths bind-mounted writable back over the read-only tree

	// seccomp (RestrictAddressFamilies=), applied by the agent as a BPF filter
	// on socket(2)/socketpair(2) before exec.
	AddressFamilies     []string // AF_* names (allowlist, or denylist when AddressFamiliesDeny)
	AddressFamiliesDeny bool     // RestrictAddressFamilies=~… : block the listed families instead

	// seccomp (SystemCallFilter=), syscall names applied by the agent as a BPF
	// filter before exec. Known @-groups (e.g. @system-service) are expanded from
	// the mirrored systemd table; an unknown @-group leaves these empty and the
	// directive is surfaced as unenforced.
	SystemCalls     []string // syscall names (allowlist, or denylist when SystemCallsDeny)
	SystemCallsDeny bool     // SystemCallFilter=~… : block the listed syscalls instead

	// Unsupported lists directives present in the unit that dew does NOT
	// enforce (unknown SystemCallFilter= @-groups, SystemCallArchitectures=,
	// partial filesystem protection, ...).
	Unsupported []string
}

// DynamicUserUID is the uid/gid dew runs a DynamicUser= service as when the
// unit names no concrete User=. systemd allocates a transient uid; dew can't,
// so it falls back to nobody — recorded as an approximation by the parser.
const DynamicUserUID = "65534"

// Confined reports whether the plan actually constrains anything.
func (p Plan) Confined() bool {
	return p.MemoryBytes > 0 || p.PidsMax > 0 || p.CPUQuota > 0 ||
		p.NeedsPrivilegeDrop() || p.ReadOnlyRoot ||
		len(p.AddressFamilies) > 0 || len(p.SystemCalls) > 0
}

// NeedsPrivilegeDrop reports whether the plan drops uid/gid, restricts the
// capability bounding set, or sets no_new_privs. The agent applies these
// natively (prctl/capset/setresuid) in the confinement shim, so unlike the
// former setpriv path this needs no extra guest binary and works on any profile.
func (p Plan) NeedsPrivilegeDrop() bool {
	return p.UID != "" || p.GID != "" || p.DynamicUser || p.DropAllCaps || len(p.DropCaps) > 0 || p.NoNewPrivs
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
	// Known SystemCallFilter= @-group tokens (e.g. @system-service) are expanded
	// from the mirrored systemd table; an UNKNOWN @-group can't be approximated,
	// so it voids the whole directive (surfaced after the scan, named below).
	var scUnknownGroups []string

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
			b, perc, err := ParseSize(val)
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
			n, perr := parseCountOrInfinity(val)
			if perr != nil {
				return p, fmt.Errorf("TasksMax=%q: %w", val, perr)
			}
			if n > 0 {
				p.PidsMax = n
			}
		case "CPUQuota":
			q, perr := parseCPUQuota(val)
			if perr != nil {
				return p, fmt.Errorf("CPUQuota=%q: %w", val, perr)
			}
			if q > 0 {
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
		case "SystemCallFilter":
			applySystemCalls(&p, val, &scUnknownGroups, note)
		case "SystemCallArchitectures":
			note("SystemCallArchitectures= (architecture restriction not enforced)")
		case "RestrictAddressFamilies":
			applyAddressFamilies(&p, val, note)
		case "ProtectSystem":
			// Only =strict (whole rootfs read-only) maps cleanly to a mount-ns
			// remount. =true/=full protect a subset (/usr,/boot[,/etc]); applying
			// strict for those would be stronger than declared, so just surface.
			if strings.EqualFold(val, "strict") {
				p.ReadOnlyRoot = true
			} else {
				note("ProtectSystem=" + val + " (only =strict is enforced, as a read-only rootfs)")
			}
		case "ReadWritePaths":
			// systemd list semantics: an empty assignment resets the list (drop-ins
			// use this to clear earlier entries), otherwise paths accumulate.
			if strings.TrimSpace(val) == "" {
				p.ReadWritePaths = nil
			} else {
				p.ReadWritePaths = append(p.ReadWritePaths, strings.Fields(val)...)
			}
		case "ProtectHome", "ReadOnlyPaths",
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
	// A SystemCallFilter= that referenced an UNKNOWN @-group can't be faithfully
	// approximated from the remaining names (dropping the group would flip the
	// effective set in the wrong direction — a denylist would under-block), so
	// enforce nothing and surface the whole directive as unenforced. Known groups
	// were already expanded inline.
	if len(scUnknownGroups) > 0 {
		p.SystemCalls = nil
		p.SystemCallsDeny = false
		slices.Sort(scUnknownGroups)
		note("SystemCallFilter= references @-group(s) " + strings.Join(slices.Compact(scUnknownGroups), " ") + " not in dew's systemd " + systemdVersion + " table; not applied")
	} else if len(p.SystemCalls) > 0 {
		// Group expansion and overlapping directives produce duplicates; sort and
		// compact for a small, deterministic wire payload (the agent dedups too).
		slices.Sort(p.SystemCalls)
		p.SystemCalls = slices.Compact(p.SystemCalls)
	}
	if p.DynamicUser && p.UID == "" {
		note("DynamicUser=yes approximated as uid " + DynamicUserUID + " (nobody)")
	}
	// Drop non-absolute ReadWritePaths here: the agent shim requires absolute
	// paths (a relative entry would mount relative to the cwd) and rejects them
	// at runtime. Surface them as unenforced now for an earlier, clearer error.
	if len(p.ReadWritePaths) > 0 {
		kept := p.ReadWritePaths[:0]
		for _, rw := range p.ReadWritePaths {
			if filepath.IsAbs(rw) {
				kept = append(kept, rw)
			} else {
				note("ReadWritePaths=" + rw + " (not absolute; ignored)")
			}
		}
		p.ReadWritePaths = kept
	}
	// ReadWritePaths only mean something atop a read-only rootfs. Without
	// ProtectSystem=strict the root stays writable, so they'd be a silent no-op
	// — surface that and drop them rather than imply they were applied.
	if len(p.ReadWritePaths) > 0 && !p.ReadOnlyRoot {
		note("ReadWritePaths= (no effect without ProtectSystem=strict; not applied)")
		p.ReadWritePaths = nil
	}
	return p, nil
}

// applyBoundingSet mirrors systemd's CapabilityBoundingSet semantics for the
// common cases: an empty assignment resets the set (drop all); a positive list
// resets to those caps (drop all, keep the listed); a leading "~" negates
// (keep the full set minus the listed caps).
func applyBoundingSet(p *Plan, val string, note func(string)) {
	if val == "" {
		// Empty assignment resets the whole set (systemd semantics): drop all,
		// discarding any caps kept or dropped by earlier assignments.
		p.DropAllCaps = true
		p.KeepCaps = nil
		p.DropCaps = nil
		return
	}
	if strings.HasPrefix(val, "~") {
		// Negation removes the named caps. If a positive set was already
		// established (DropAllCaps with KeepCaps), remove them from it so a
		// later ~CAP_X actually revokes CAP_X; otherwise it's "keep all except
		// these" applied to the full inherited set (DropCaps).
		for _, c := range strings.Fields(strings.TrimPrefix(val, "~")) {
			cap := strings.ToLower(c)
			if p.DropAllCaps {
				p.KeepCaps = slices.DeleteFunc(p.KeepCaps, func(x string) bool { return x == cap })
			} else {
				p.DropCaps = append(p.DropCaps, cap)
			}
		}
		return
	}
	p.DropAllCaps = true // start from empty, then add the listed caps back
	for _, c := range strings.Fields(val) {
		p.KeepCaps = append(p.KeepCaps, strings.ToLower(c))
	}
}

// applyAddressFamilies parses RestrictAddressFamilies=. An empty assignment
// resets the list (systemd drop-in semantics); a leading ~ is the denylist form;
// otherwise it's an allowlist. Names are normalised to upper-case AF_* (the
// agent resolves them). dew supports a single polarity — if a later assignment
// flips allow↔deny, the combined semantics are non-trivial, so it keeps the new
// form and surfaces the approximation.
func applyAddressFamilies(p *Plan, val string, note func(string)) {
	if val == "" {
		p.AddressFamilies = nil
		p.AddressFamiliesDeny = false
		return
	}
	deny := strings.HasPrefix(val, "~")
	if len(p.AddressFamilies) > 0 && p.AddressFamiliesDeny != deny {
		note("RestrictAddressFamilies= mixes allow and deny forms (approximated by the last form)")
		p.AddressFamilies = nil
	}
	p.AddressFamiliesDeny = deny
	for _, f := range strings.Fields(strings.TrimPrefix(val, "~")) {
		p.AddressFamilies = append(p.AddressFamilies, strings.ToUpper(f))
	}
}

// applySystemCalls parses SystemCallFilter=. An empty assignment resets the
// list; a leading ~ is the denylist form; otherwise it's an allowlist. Explicit
// syscall names accumulate (normalised to lower-case); a known @-group token is
// expanded via systemdSyscallGroups (the mirrored systemd table, 5c) into its
// member names, while an unknown @-group is appended to *unknownGroups so the
// caller can void the whole directive and name the offending tokens — dropping
// one silently would flip the effective set in the wrong direction (a denylist
// would under-block). Like address families, dew supports a single polarity and
// keeps the last form if a later assignment flips it.
func applySystemCalls(p *Plan, val string, unknownGroups *[]string, note func(string)) {
	if val == "" {
		// Reset discards prior entries, including any unknown-group state, so a
		// later explicit/known-group directive can still be enforced.
		p.SystemCalls = nil
		p.SystemCallsDeny = false
		*unknownGroups = nil
		return
	}
	deny := strings.HasPrefix(val, "~")
	if (len(p.SystemCalls) > 0 || len(*unknownGroups) > 0) && p.SystemCallsDeny != deny {
		// Polarity flip: dew keeps the last form, so drop prior entries and the
		// prior unknown-group state with them. Gate on either accumulator — a prior
		// form of only unknown @-groups leaves SystemCalls empty but must still flip.
		note("SystemCallFilter= mixes allow and deny forms (approximated by the last form)")
		p.SystemCalls = nil
		*unknownGroups = nil
	}
	p.SystemCallsDeny = deny
	for _, s := range strings.Fields(strings.TrimPrefix(val, "~")) {
		if strings.HasPrefix(s, "@") {
			members, ok := systemdSyscallGroups[strings.ToLower(s)]
			if !ok {
				*unknownGroups = append(*unknownGroups, strings.ToLower(s))
				continue
			}
			p.SystemCalls = append(p.SystemCalls, members...)
			continue
		}
		// systemd allows "name:errno" to override the action per syscall; dew
		// applies one action, so keep just the name. Skip empties (a stray ":"
		// or ":errno" with no name).
		name, _, _ := strings.Cut(s, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		p.SystemCalls = append(p.SystemCalls, name)
	}
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "yes", "true", "1", "on":
		return true
	}
	return false
}

// ParseSize parses a systemd size (base-1024 K/M/G/T/P suffix, or bare bytes).
// Returns (bytes, isPercentage, error). "infinity"/"" → (0,false,nil). Exported
// so the host --cgroup parser shares one size grammar with --confine.
func ParseSize(s string) (int64, bool, error) {
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
	if err != nil || n <= 0 {
		// Reject 0 (and 0K/0M/…) so MemoryMax=0 fails like a non-positive
		// TasksMax/CPUQuota rather than being silently treated as unlimited.
		return 0, false, fmt.Errorf("invalid size")
	}
	if n > (1<<63-1)/mult {
		return 0, false, fmt.Errorf("size too large")
	}
	return n * mult, false, nil
}

// parseCountOrInfinity parses a positive integer; "infinity"/"" mean unset
// (0, nil). A malformed or non-positive value is an error so an intended hard
// cap isn't silently dropped.
func parseCountOrInfinity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "infinity") {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid count")
	}
	return n, nil
}

// parseCPUQuota converts systemd's "N%" into a cpu.max quota for a 100000us
// period (100% == one core == 100000). "infinity"/"" mean unset (0, nil); a
// malformed value or one that rounds to a 0 quota is an error.
func parseCPUQuota(s string) (int64, error) {
	const period = 100000
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "infinity") {
		return 0, nil
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil || pct <= 0 {
		return 0, fmt.Errorf("invalid percentage")
	}
	return QuotaFromFloat(pct / 100 * period)
}

// QuotaFromFloat converts a computed cpu.max quota to int64, rejecting values
// that overflow int64 (a finite-but-huge float, or +Inf — both > MaxInt64) and
// values that round to 0 (NaN lands here too, since int64(NaN) == 0). Exported
// so the host --cgroup parser shares this guard.
func QuotaFromFloat(f float64) (int64, error) {
	if f > float64(1<<63-1) {
		return 0, fmt.Errorf("cpu quota out of range")
	}
	q := int64(f)
	if q == 0 {
		return 0, fmt.Errorf("cpu quota too small (rounds to 0)")
	}
	return q, nil
}
