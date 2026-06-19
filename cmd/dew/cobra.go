//go:build darwin

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/solcreek/dew/internal/selfupdate"
	"github.com/solcreek/dew/pkg/dewerr"
)

// Incremental cobra migration. Commands move from the hand-rolled
// dispatcher in main() to this cobra root one at a time; cobraCommands
// lists the ones already migrated. The passthrough commands (exec, run,
// logs) go first because they're the ones the custom parser handled
// specially — `SetInterspersed(false)` is what stops cobra from eating
// a guest command's own flags (e.g. `dew exec curl --json url`).
var cobraCommands = map[string]bool{
	"exec":     true,
	"run":      true,
	"logs":     true,
	"up":       true,
	"down":     true,
	"build":    true,
	"deploy":   true,
	"rollback": true,
	"share":    true,
	"services": true,
	"assets":   true,
	"auth":     true,
	"env":      true,
	"serve":    true,
	"doctor":   true,
	"update":   true,
	// deprecated single-level aliases (still work; print a nudge)
	"start":   true,
	"status":  true,
	"forward": true,
	// namespaces — shimmed to cmdVM/cmdServer, which keep their own
	// subcommand dispatch (and usage errors that vm_test.go pins).
	"vm":     true,
	"server": true,
	// terminal: print version; removed-command stubs.
	"version": true,
	"install": true,
	"app":     true,
	"apps":    true,
	"session": true,
}

// passthroughCommands take a guest command after their own flags. For
// these, main()'s global flag pre-scan must NOT look past the
// subcommand token, or a guest command's own --json/--events would be
// mistaken for dew's. They parse their own dew flags, so nothing is
// lost by limiting the pre-scan to leading globals.
var passthroughCommands = map[string]bool{
	"exec": true,
	"run":  true,
	"logs": true,
}

// dispatchCobra runs a command through the cobra root and reports
// whether the root recognised it (i.e. cmd is in cobraCommands). When
// it returns handled=false, main() renders the unknown-command error —
// there is no legacy switch any more. The returned error funnels
// through main()'s existing exit-code mapper.
func dispatchCobra(cmd string, subArgs []string) (handled bool, err error) {
	if !cobraCommands[cmd] {
		return false, nil
	}
	root := newRootCmd()
	root.SetArgs(append([]string{cmd}, subArgs...))
	return true, root.Execute()
}

// globalFlagScanArgs returns the slice main() scans for leading global
// no-value flags (--json/--events/...). For a passthrough command it
// excludes the guest command (everything from the subcommand token on,
// i.e. dispatchArgs), so the guest's own flags aren't read as dew's.
// allArgs is os.Args[1:]; dispatchArgs is it with leading globals stripped.
func globalFlagScanArgs(allArgs, dispatchArgs []string, passthrough bool) []string {
	if passthrough {
		return allArgs[:len(allArgs)-len(dispatchArgs)]
	}
	return allArgs
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use: "dew",
		// main() owns top-level usage and error rendering (consistent
		// exit codes via dewerr); keep cobra from printing its own.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Map every flag-parse error (unknown flag, bad value) to a dewerr
	// usage error so main()'s exit-code mapper returns 2, not the generic
	// 1. Set on the root; cobra inherits it to every subcommand, so
	// native commands (exec, logs, …) don't each need their own.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return dewerr.Newf(dewerr.CodeUsage, "%v", err)
	})
	root.AddCommand(newExecCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newLogsCmd())

	// Leaf commands: cobra owns the command tree; their own arg parsing
	// stays in cmdXxx (DisableFlagParsing passes tokens through verbatim).
	// Short text feeds the eventual `dew --help` tree; per-command --help
	// is still served by main()'s namespace-aware interception for now.
	root.AddCommand(legacyShim("up [dir]", "Start a dev environment (auto-detect project)", cmdUp))
	root.AddCommand(legacyShim("down", "Stop the running dev VM", cmdDown))
	root.AddCommand(legacyShim("build [dir]", "Package the current project for deployment", cmdBuild))
	root.AddCommand(legacyShim("deploy <target>", "Deploy a built tarball to a remote server", cmdDeploy))
	root.AddCommand(legacyShim("rollback <target>", "Roll back a remote deployment", cmdRollback))
	root.AddCommand(legacyShim("share [port]", "Expose a local port via a public HTTPS URL", cmdShare))
	root.AddCommand(legacyShim("services", "List services and their connection strings", cmdServices))
	root.AddCommand(legacyShim("assets", "Manage cached VM images (pull/list/path)", cmdAssets))
	root.AddCommand(legacyShim("auth", "Manage deploy credentials", cmdAuth))
	root.AddCommand(legacyShim("env", "Manage deployment environment variables", cmdEnv))
	root.AddCommand(legacyShim("serve", "Run the deploy receiver", cmdServe))
	root.AddCommand(legacyShim("doctor", "Diagnose the local environment", cmdDoctor))
	root.AddCommand(legacyShim("update", "Update dew to the latest release", func([]string) error { return selfupdate.Update(version) }))

	// Namespaces. Shimmed to cmdVM/cmdServer, which keep their own
	// subcommand dispatch (vm start/stop/status/forward,
	// server create/list/destroy) and usage errors.
	root.AddCommand(legacyShim("vm <start|stop|status|list|forward> [args...]", "Manage long-lived VMs", cmdVM))
	root.AddCommand(legacyShim("server <create|list|destroy> [args...]", "Manage remote deploy targets", cmdServer))

	// Deprecated single-level aliases — keep working, print a nudge.
	root.AddCommand(legacyShim("start", "Deprecated: use `dew vm start`", func(a []string) error {
		deprecationHint("start", "vm start")
		return cmdStart(a)
	}))
	root.AddCommand(legacyShim("status", "Deprecated: use `dew vm status`", func(a []string) error {
		deprecationHint("status", "vm status")
		return cmdStatus(a)
	}))
	root.AddCommand(legacyShim("forward", "Deprecated: use `dew vm forward`", func(a []string) error {
		deprecationHint("forward", "vm forward")
		return cmdForward(a)
	}))

	root.AddCommand(legacyShim("version", "Print the dew version", func([]string) error {
		fmt.Printf("dew %s\n", version)
		return nil
	}))

	// Removed commands: keep a clear usage error so scripts that survived
	// the deprecation window fail fast and obvious.
	appsStub := legacyShim("install", "Removed in v0.7.20", func([]string) error {
		return dewerr.New(dewerr.CodeUsage,
			"dew install/app/apps was removed in v0.7.20.\n"+
				"The pre-packaged apps catalog now lives in a separate tool.\n"+
				"For arbitrary container workloads in dew, use: dew run --network -- <cmd>")
	})
	appsStub.Aliases = []string{"app", "apps"}
	root.AddCommand(appsStub)

	root.AddCommand(legacyShim("session", "Removed in v0.7.18", func([]string) error {
		return dewerr.New(dewerr.CodeUsage,
			"dew session was removed in v0.7.18 — it stored state in-process and `session exec` could never find the VM.\n"+
				"For persistent VMs use `dew up` (project) or `dew vm start` (manual profile) — both register with the daemon and `dew exec` works against them.")
	}))
	return root
}

// legacyShim wraps a command whose flag/arg parsing still lives in its
// cmdXxx handler. DisableFlagParsing makes cobra pass every token
// through unchanged, so behavior is identical to the old switch dispatch
// while the command joins the cobra tree. The first word of use is the
// command name.
func legacyShim(use, short string, run func([]string) error) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return run(args)
		},
	}
}

// newRunCmd is a thin shim: run shares the big parseFlags() parser with
// `vm start` and `up` (--cpus/--memory/--profile/--image/--network/...),
// so flag handling stays in cmdRun for now. DisableFlagParsing makes
// cobra pass every token through verbatim — including the `--` separator
// that parseFlags relies on to mark the start of a dash-leading guest
// command. A later slice can lift the shared flags onto cobra natively.
func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "run [flags] -- <cmd...>",
		Short:              "Run a command in an ephemeral VM",
		Long:               "Run a command in a fresh, throwaway VM. State does NOT persist between\ninvocations; use `dew vm start` + `dew exec` for a long-lived VM.",
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdRun(args)
		},
	}
}

// newLogsCmd prints a --with service's container log. Native command
// with a single service-name positional and a --json flag (the JSON
// result envelope, consistent with the other commands).
func newLogsCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "logs [--json] <service>",
		Short: "Show a --with service's container logs",
		Long:  "Print the container log for a --with service (postgres, redis, …),\nwhich runs via crun in the guest at /var/log/dew-oci-<name>.log.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 {
				return dewerr.New(dewerr.CodeUsage, "usage: dew logs [--json] <service>")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOut {
				flagJSON = true
			}
			return cmdLogs(args)
		},
	}
	// Accept --json like the other commands (it would otherwise be an
	// "unknown flag" error). cmdLogs runs through the exec path, which
	// honors flagJSON and emits the result envelope.
	c.Flags().BoolVar(&jsonOut, "json", false, "emit a JSON result envelope on stdout")
	return c
}

func newExecCmd() *cobra.Command {
	var jsonOut bool
	var timeout time.Duration
	var vmName string
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
			if err := validateVMName(vmName); vmName != "" && err != nil {
				return err
			}
			flagVMName = vmName
			return runExecRequest(args, jsonOut || flagJSON, int(timeout/time.Millisecond))
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "emit a JSON result envelope on stdout")
	c.Flags().DurationVar(&timeout, "timeout", 0, "guest exec timeout, e.g. 5m or 300s (default: agent's 30s)")
	c.Flags().StringVar(&vmName, "name", "", "target a named VM (default: the unnamed VM)")
	// THE load-bearing line: stop parsing dew flags at the first
	// positional so a guest command's own flags pass through untouched.
	c.Flags().SetInterspersed(false)
	// Flag-parse errors map to CodeUsage via the root's FlagErrorFunc
	// (inherited), so no per-command override is needed here.
	return c
}
