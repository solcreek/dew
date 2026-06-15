//go:build darwin

package main

import (
	"fmt"
	"os"
	"strings"
)

// subcommandHelp is the per-command help text printed when a user
// runs `dew <subcommand> --help`. Each entry is the short usage,
// followed by relevant flags, followed by 1-3 representative
// invocations. Aim for ~20 lines max — agents and humans should
// be able to read the whole block at a glance.
//
// When adding a new subcommand, prefer adding it here over
// burying flags in source. The fresh-eyes agent report flagged
// "no per-subcommand help" as the #1 discoverability barrier.
var subcommandHelp = map[string]string{
	"up": `dew up — start a dev environment with auto-detected project

Usage:
  dew up [dir] [flags]

The project directory is mounted into the guest at /app (read-write,
live sync via virtiofs). The dev server runs there; package
managers install into a cached /app/node_modules that survives
` + "`dew down`" + ` (see CHANGELOG v0.7.17 for cache details). Use ` + "`dew exec`" + `
to run commands against the running VM after ` + "`dew up`" + `.

Flags:
  --profile minimal|node|python|standard
                Override auto-detected profile.
  --with <services>
                Comma-separated services to start (postgres, redis,
                mysql, mongo, minio).
  --services-only
                Boot only the --with services; skip project detection
                and the dev server. Requires --with. (alias: --no-dev)
  --dry-run     Print the plan (project, profile, install/dev
                commands, ports) and exit without booting.
  --json        Emit lifecycle events as NDJSON; final {"type":"ready"}
                event carries url/port/framework.
  --events      Same as --json but streamed (NDJSON-only output).
  --cpus N --memory MB
                Override defaults (1 CPU / 512 MB minimum, profile
                may raise these).

Examples:
  dew up                              # auto-detect this directory
  dew up ./apps/web                   # different project dir
  dew up --with postgres,redis        # add services
  dew up --dry-run --json | jq        # preview without booting
`,
	"run": `dew run — execute a command in an ephemeral VM

Usage:
  dew run [flags] -- <cmd> [args...]
  dew run "<shell string>"

Argv form (after --): args are passed straight to the guest, no
shell wrap. Single-string form: wrapped in /bin/sh -c so shell
metacharacters work.

The VM is ephemeral — each ` + "`dew run`" + ` boots a fresh VM and tears
it down on exit. Packages you install, files you write outside
--share mounts, and any other state DO NOT persist across runs.
For persistent state, start a VM once with ` + "`dew vm start`" + ` (or
` + "`dew up`" + ` for a project), then attach with ` + "`dew exec`" + ` as many
times as you want.

Without --share, the guest has no view of host files. With
--share <hostdir>, the host directory appears at /<tag> inside
the guest (default tag is the directory's basename).

Flags:
  --profile minimal|node|python|standard
  --network              Enable guest networking (off by default).
  --network-policy open|restricted     See dew help for details.
  --share <hostdir>[:rw|:ro]   or   --share <tag>:<hostdir>[:rw|:ro]
                         Mount a host directory into the guest.
                         Default mode is read-only.
  --json                 Pass guest exit code in JSON; dew exits 0.
  --stream / --events    Stream stdout/stderr live.
  --timeout DUR          Overall wall-clock bound for the whole run
                         (boot + agent wait + exec), e.g. 90s, 5m.
                         On expiry the VM is stopped and dew exits
                         with the timeout code (104).

Examples:
  dew run -- uname -a
  dew run --timeout 90s -- uname -a
  dew run -- sh -c 'echo A; echo B'
  dew run --network -- curl https://example.com
  dew run --share ./data:rw -- ls /data
`,
	"exec": `dew exec — execute a command in a running VM

Usage:
  dew exec [flags] <cmd> [args...]

Requires a VM started by ` + "`dew up`" + ` or ` + "`dew vm start`" + `. Same
argv rules as ` + "`dew run`" + ` — argv when given multiple args, shell-
wrap for single string.

Flags:
  --json     Wrap output in the standard envelope; dew exits 0,
             guest exit code lives in data.guest_exit_code.

Examples:
  dew exec -- ls /app
  dew exec -- sh -c 'echo hi; date'
  dew exec --json -- npm test
`,
	"start": `dew vm start — boot a VM without running a command

Usage:
  dew vm start [flags]

Profile must be specified explicitly (no project detection):
  --profile minimal|node|python|standard

The VM registers with the daemon at ~/.local/state/dew/default.sock,
so dew exec can attach to it. Networking is on by default; pass
--network-policy=restricted to lock down outbound. Use dew vm stop
(or its alias dew down) to stop.

Flags for scripted/agent use:
  --json / --events      Emit one NDJSON ready event on stdout once
                         the daemon socket accepts connections:
                         {"type":"ready","socket":...,"pid":...,
                          "profile":...,"elapsed_ms":...}
  --timeout DUR          Bound the path to readiness (boot + agent
                         handshake). On expiry the VM is stopped and
                         dew exits with the timeout code (104).
                         After ready, the process stays in the
                         foreground as usual.

Wait-until-usable loop:
  dew vm start --profile standard --json --timeout 60s > events.ndjson &
  until grep -q '"type":"ready"' events.ndjson; do sleep 0.5; done
(or poll "dew vm status --json" for phase/running.)

Examples:
  dew vm start --profile minimal
  dew vm start --profile standard --forward 8090:8090

Note: ` + "`dew start`" + ` is the legacy alias for this command and is
slated for removal in v0.9.x.
`,
	"down": `dew down — stop the running VM

Usage:
  dew down

No flags. Removes the daemon socket and shuts down the VM that
dew up or dew vm start brought up.
`,
	"deploy": `dew deploy — push a built tarball to a remote dew server

Usage:
  dew deploy <target> [flags]

Flags:
  --tarball <path>  Override auto-detected tarball.
  --image <name>    Deploy a container image instead.
  --app <name>      App name on the target (default: tarball basename).
  --dry-run         Validate target/auth and exit without uploading.
  --json            Emit deploy events as JSON.

Examples:
  dew deploy myhost.example.com
  dew deploy --image ghcr.io/me/api:v1 --app api myhost
  dew deploy myhost --dry-run
`,
	"build": `dew build — package the current project for deployment

Usage:
  dew build [dir] [flags]

Flags:
  -o, --output <path>   Override default <app>.tar.gz
  --json                Print the manifest to stdout.
`,
	"share": `dew share — expose a local port via a public HTTPS URL

Usage:
  dew share [port]

The port defaults to 3000. Returns a temporary public URL backed by
a tunnel implementation; the tunnel dies when you ^C.

Output modes:
  --json     Single-shot JSON on stdout once the URL is ready:
               {"url":"https://...","port":"3000"}
  --events   NDJSON event stream — one JSON object per line — for
             agents and other tools that want to react to tunnel
             lifecycle. Events fire in this order:
               starting       — share invoked; port validated
               tunnel-url     — URL obtained (load-bearing event)
               established    — HTTP probe confirmed traffic
               probe-timeout  — probe ran out before 2xx/3xx
               closed         — tunnel process exited
             Each event has ` + "`event`" + ` and ` + "`ts`" + ` (RFC3339Nano);
             additional fields documented per event in the source.

Examples:
  dew share 3000
  dew share --events 3000 | jq 'select(.event=="established")'
`,
	"services": `dew services — list services and their connection strings

Usage:
  dew services [--json]

Lists the predefined services (postgres, redis, mysql, mongo, minio),
their default credentials as ready-to-use connection strings, and —
when a VM is running — which are live and on what host port (which may
differ from the default after a busy-port fallback).

Examples:
  dew services
  dew services --json | jq '.data.services[] | select(.running)'
`,
	"logs": `dew logs — show a service's container logs

Usage:
  dew logs <service>

Prints the container logs for a --with service (postgres, redis, …),
which run via crun in the guest. Saves you from knowing the guest log
path or that services run under crun.

Examples:
  dew logs postgres
  dew logs mysql
`,
	"vm": `dew vm — manage a long-lived VM

Usage:
  dew vm <start|stop|status|forward> [args...]

Subcommands:
  start         Boot a VM without running a command (see
                ` + "`dew vm start --help`" + `).
  stop          Stop the running VM (alias: dew down).
  status        Show the running VM's state.
  forward       Manage host→guest port forwards at runtime:
                  dew vm forward add  HOST:GUEST
                  dew vm forward remove HOST:GUEST
                  dew vm forward list
                A busy host port falls back to a free one; the
                actual port is reported.

Examples:
  dew vm start --profile node
  dew vm forward add 5432:5432
  dew vm forward list
`,
	"server": `dew server — manage remote dew deploy targets

Usage:
  dew server <create|list|destroy> [args...]

Subcommands:
  create        Provision a new remote dew server.
  list          List configured servers.
  destroy       Tear down a server.
`,
}

// printSubcommandHelp prints the help block for subcommand and
// returns true if one was printed. The caller exits afterwards.
func printSubcommandHelp(subcommand string) bool {
	text, ok := subcommandHelp[subcommand]
	if !ok {
		return false
	}
	fmt.Fprint(os.Stderr, text)
	return true
}

// helpKeyCandidates returns the help-map keys to try for a command
// path, most specific first. For `dew vm start` it yields
// ["vm start", "start", "vm"]; for `dew up` just ["up"]. This makes
// two-level commands resolve their help instead of falling through to
// a flag-parse error (`dew vm start --help` used to print "unknown
// flag" because the dispatcher only ever looked up the first token).
func helpKeyCandidates(cmd string, subArgs []string) []string {
	if len(subArgs) > 0 && !strings.HasPrefix(subArgs[0], "-") {
		return []string{cmd + " " + subArgs[0], subArgs[0], cmd}
	}
	return []string{cmd}
}

// printSubcommandHelpPath prints the most specific matching help block
// for a command path and reports whether anything was printed.
func printSubcommandHelpPath(cmd string, subArgs []string) bool {
	for _, k := range helpKeyCandidates(cmd, subArgs) {
		if printSubcommandHelp(k) {
			return true
		}
	}
	return false
}
