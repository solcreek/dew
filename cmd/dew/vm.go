//go:build darwin

package main

import (
	"fmt"
	"os"

	"github.com/solcreek/dew/pkg/dewerr"
)

// cmdVM is the namespace dispatcher for VM lifecycle commands:
// start, stop, status, forward. It delegates to the same handler
// functions as the legacy top-level aliases (`dew start`, etc.)
// so behavior is identical — only the entry path differs.
//
// The namespace exists to disambiguate dew's two personalities:
//   - `dew up [dir]`: project dev workload (high-level, like compose up)
//   - `dew vm <verb>`: generic VM primitive (low-level, like podman machine)
//
// Both are valid first-class entry points; the namespace makes the
// distinction visible at the CLI surface instead of hidden in flag
// defaults.
//
// Each verb accepts `--name <vm>` to target a named, concurrently
// running VM (its own <name>.sock and <name>/ state dir). Omitting it
// targets the default unnamed VM, so existing callers are unaffected.
func cmdVM(args []string) error {
	if len(args) == 0 {
		return dewerr.New(dewerr.CodeUsage,
			"usage: dew vm <start|stop|status|forward> [--name <vm>] [args...]")
	}
	switch args[0] {
	case "start":
		return cmdStart(args[1:])
	case "stop":
		return cmdDown(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "forward":
		return cmdForward(args[1:])
	default:
		return dewerr.Newf(dewerr.CodeUsage, "dew vm: unknown subcommand %q", args[0])
	}
}

// deprecationHint prints a one-line stderr nudge when callers reach
// an infra command via its pre-namespace path. We don't error on
// deprecation — old callers must keep working until the v0.9.x
// release that removes the aliases. Suppressed in --json mode so
// agents that already parse our envelopes don't get surprise stderr.
func deprecationHint(old, new string) {
	if flagJSON {
		return
	}
	fmt.Fprintf(os.Stderr, "dew: `dew %s` is deprecated; use `dew %s` (removed in v0.9.x)\n", old, new)
}
