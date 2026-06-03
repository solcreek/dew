//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/solcreek/dew/internal/daemon"
)

// cmdStatus answers "is dew running?" without side effects. Many
// callers (humans surveying their machine, agents deciding whether
// to `dew start`, grove engine indicator) want this fact alone; the
// existing daemon protocol only exposes it via attempted-exec which
// is heavyweight and noisy in logs.
//
// The check is two-stage:
//   1. socket file exists at the canonical path
//   2. socket actually accepts a connection (otherwise it's stale,
//      left behind by a crash)
//
// Both halves report independently so the user sees the truth even
// in the rare crash-leftover case. Exit is always 0 — "not running"
// is a legitimate state, not an error.
func cmdStatus(args []string) error {
	sockPath := daemon.SocketPath("")

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

	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data": map[string]any{
				"running":        alive,
				"socket":         sockPath,
				"socket_present": socketPresent,
			},
		})
		return nil
	}

	switch {
	case alive:
		fmt.Printf("dew: running\n  socket: %s\n", sockPath)
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
	_ = args // status takes no arguments today; reserve for future filters
	return nil
}
