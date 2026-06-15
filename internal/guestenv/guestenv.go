// Package guestenv builds the environment for commands the guest agent
// executes. It lives in its own (build-tag-free) package so the PATH
// logic is unit-testable on any host, while the agent that uses it is
// linux-only.
package guestenv

import "strings"

// DefaultPath is the PATH guaranteed for guest exec when the agent's
// own environment provides none. It covers the sbin dirs (ss, ip — the
// usual "ss: not found" culprits), /usr/local/bin (node, crun), and the
// busybox dirs.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// ExecEnv builds the environment for a guest-executed command. It
// starts from base (typically os.Environ()), guarantees a non-empty
// PATH — injecting DefaultPath when base has none or an empty one — and
// appends extra (per-request overrides) last so a caller-supplied
// PATH still wins.
//
// The agent previously left cmd.Env nil whenever the request carried no
// env, so commands inherited whatever PATH the agent process happened
// to boot with — frequently empty, which made bare names like `ss`
// resolve intermittently or not at all.
func ExecEnv(base, extra []string) []string {
	hasPath := false
	out := make([]string, 0, len(base)+len(extra)+1)
	for _, e := range base {
		if strings.HasPrefix(e, "PATH=") {
			if e == "PATH=" {
				// Empty PATH is as good as none — drop it and inject the
				// default below.
				continue
			}
			hasPath = true
		}
		out = append(out, e)
	}
	if !hasPath {
		out = append(out, "PATH="+DefaultPath)
	}
	out = append(out, extra...)
	return out
}
