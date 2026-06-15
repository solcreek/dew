//go:build darwin

package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/solcreek/dew/pkg/dewerr"
)

// Incremental cobra migration. Commands move from the hand-rolled
// dispatcher in main() to this cobra root one at a time; cobraCommands
// lists the ones already migrated. The passthrough commands (exec, run,
// logs) go first because they're the ones the custom parser handled
// specially — `SetInterspersed(false)` is what stops cobra from eating
// a guest command's own flags (e.g. `dew exec curl --json url`).
var cobraCommands = map[string]bool{
	"exec": true,
}

// dispatchCobra runs a migrated command through cobra and reports
// whether it handled it. main() falls back to the legacy switch for
// anything not in cobraCommands. The returned error funnels through
// main()'s existing exit-code mapper.
func dispatchCobra(cmd string, subArgs []string) (handled bool, err error) {
	if !cobraCommands[cmd] {
		return false, nil
	}
	root := newRootCmd()
	root.SetArgs(append([]string{cmd}, subArgs...))
	return true, root.Execute()
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use: "dew",
		// main() owns top-level usage and error rendering (consistent
		// exit codes via dewerr); keep cobra from printing its own.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newExecCmd())
	return root
}

func newExecCmd() *cobra.Command {
	var jsonOut bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "exec [flags] <cmd...>",
		Short: "Execute a command in the running VM",
		Long: "Execute a command in the running VM (started by `dew up` or\n" +
			"`dew vm start`). With 2+ arguments the command runs as argv with no\n" +
			"shell wrap; a single string is run via /bin/sh -c. Flags after the\n" +
			"command belong to the command, not to dew.",
		// Custom Args + flag-error funcs return dewerr.CodeUsage so
		// main()'s exit-code mapper gives usage errors code 2 (parity with
		// the legacy parser), not the generic code 1 cobra would yield.
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 {
				return dewerr.New(dewerr.CodeUsage, "usage: dew exec [--timeout DUR] <cmd...>")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOut {
				// Keep the global in sync so downstream JSON-mode checks
				// (selfupdate skip, error envelope) behave as before.
				flagJSON = true
			}
			return runExecRequest(args, jsonOut || flagJSON, int(timeout/time.Millisecond))
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "emit a JSON result envelope on stdout")
	c.Flags().DurationVar(&timeout, "timeout", 0, "guest exec timeout, e.g. 5m or 300s (default: agent's 30s)")
	// THE load-bearing line: stop parsing dew flags at the first
	// positional so a guest command's own flags pass through untouched.
	c.Flags().SetInterspersed(false)
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return dewerr.Newf(dewerr.CodeUsage, "%v", err)
	})
	return c
}
