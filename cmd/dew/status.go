//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/internal/vmstate"
	"github.com/solcreek/dew/pkg/dewerr"
)

// cmdStatus answers "is dew running?" without side effects. Many
// callers (humans surveying their machine, agents deciding whether
// to `dew start`, grove engine indicator) want this fact alone; the
// existing daemon protocol only exposes it via attempted-exec which
// is heavyweight and noisy in logs.
//
// Two sources, complementary:
//   - the daemon socket: authority for "exec is available" — exists
//     only after `dew vm start`/`dew up` finish the token handshake
//   - the vmstate file: covers what the socket can't see — a VM still
//     booting, or an ephemeral `dew run` (which never opens a socket).
//     A state file whose PID is dead is a crash leftover and ignored.
//
// Exit code is a reuse-vs-boot gate, not a success/failure flag: 0 when a VM
// is present (running, booting, or an ephemeral `dew run`), CodeConflict when
// none is — so `if dew vm status; then reuse; else boot; fi` works and never
// starts a second VM over one that is still booting. The human/JSON report is
// the command's sole output; statusExit only carries the code (see main()).
// (Note: a bare `dew vm status` under `set -e` now aborts when nothing runs;
// gate it in an `if`/`||` or read the --json `running` field instead.)
func cmdStatus(args []string) error {
	if _, err := popNameFlag(args); err != nil {
		return err
	}
	sockPath := daemon.SocketPath(flagVMName)

	_, statErr := os.Stat(sockPath)
	socketPresent := statErr == nil

	alive := false
	if socketPresent {
		conn, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			alive = true
		}
	}

	stateDir := vmstate.DirFor(daemon.SocketDir(), flagVMName)
	st, hasState := vmstate.Read(stateDir)
	if hasState && !vmstate.Alive(st.PID) {
		// Crash leftover: the owning process is gone. Self-heal so the
		// next status call doesn't re-evaluate it.
		vmstate.Clear(stateDir, st.PID)
		hasState = false
	}

	present := statusVMPresent(alive, hasState, st.Phase)

	if flagJSON {
		data := map[string]any{
			"running":        alive,
			"socket":         sockPath,
			"socket_present": socketPresent,
		}
		if hasState {
			data["phase"] = string(st.Phase)
			data["pid"] = st.PID
			data["mode"] = st.Mode
			if st.Profile != "" {
				data["profile"] = st.Profile
			}
			data["started_at"] = st.StartedAt.Format(time.RFC3339)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data":           data,
		})
	} else {
		since := ""
		if hasState {
			since = time.Since(st.StartedAt).Round(time.Second).String()
		}
		switch {
		case alive:
			fmt.Printf("dew: running\n  socket: %s\n", sockPath)
			if hasState {
				fmt.Printf("  mode: dew %s (pid %d, up %s)\n", st.Mode, st.PID, since)
			}
		case hasState && st.Phase == vmstate.PhaseBooting:
			fmt.Printf("dew: booting (dew %s, pid %d, %s elapsed)\n", st.Mode, st.PID, since)
			fmt.Printf("  The VM is starting; status flips to running once the guest agent answers.\n")
		case hasState && st.Phase == vmstate.PhaseRunning:
			// Reachable guest but no daemon socket: an ephemeral `dew run`
			// in flight (it never opens one), or a daemon that lost its
			// socket. Either way exec via `dew exec` is not available.
			fmt.Printf("dew: running (ephemeral dew %s, pid %d, up %s)\n", st.Mode, st.PID, since)
			fmt.Printf("  No daemon socket — `dew exec` won't attach to this VM.\n")
		case socketPresent:
			// Stale socket — the daemon crashed without cleanup.
			// `dew start` recovers from this automatically; surface it
			// so the user understands why "not running" coexists with
			// a present socket file.
			fmt.Printf("dew: not running (stale socket at %s)\n", sockPath)
			fmt.Printf("  Start: dew vm start --profile standard\n")
		default:
			fmt.Printf("dew: not running\n")
			fmt.Printf("  Start: dew vm start --profile standard\n")
		}
	}

	if !present {
		// CodeConflict is what `dew exec` / `dew vm forward` already return for
		// "no running VM" (and docs/exit-codes.md maps "the VM isn't running
		// yet" to it); match them. The report above already went to stdout, so
		// this only sets the exit code.
		return statusExit{Code: dewerr.CodeConflict}
	}
	return nil
}

// statusVMPresent reports whether `dew vm status` should treat a VM as present
// — running (daemon-backed), booting, or an ephemeral `dew run` — as opposed
// to absent (nothing, or only a stale socket). It drives the exit code: present
// → 0, absent → CodeConflict, so a reuse-vs-boot gate never starts a second VM
// over one that is still booting.
func statusVMPresent(alive, hasState bool, phase vmstate.Phase) bool {
	if alive {
		return true
	}
	return hasState && (phase == vmstate.PhaseBooting || phase == vmstate.PhaseRunning)
}

// statusExit lets a query command (dew vm status) set the process exit code
// from the state it reported without the dispatcher printing an error banner:
// the command already wrote its own human or JSON output, and "not running" is
// a query result, not a failure to announce. main() recognizes this type,
// exits with Code, and prints nothing further.
type statusExit struct{ Code dewerr.Code }

func (statusExit) Error() string { return "not running" }
