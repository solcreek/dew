//go:build darwin

package main

import (
	"strings"
	"time"

	"github.com/solcreek/dew/internal/services"
)

// Readiness polling cadence shared by `dew up` and `dew run`. A container
// typically binds its port in well under a second, so poll at a fine interval
// rather than a flat 1s — under the old 1s a service ready at ~180ms still
// waited up to a full second to be detected (and that cost was paid per
// service when they were started serially). readyProbeAttempts keeps a ~30s
// overall budget for a genuinely slow first start (cold image, DB init).
const (
	readyProbeInterval = 100 * time.Millisecond
	readyProbeAttempts = 300
)

// waitGuestReady calls probe repeatedly until it returns true or the
// attempt budget is exhausted, sleeping interval between tries. probe
// is injected (rather than calling the guest directly) so the readiness
// polling is unit-testable without a running VM. Returns true once the
// service is confirmed ready, false on timeout.
func waitGuestReady(probe func() bool, attempts int, interval time.Duration) bool {
	for i := 0; i < attempts; i++ {
		if probe() {
			return true
		}
		if i < attempts-1 {
			time.Sleep(interval)
		}
	}
	return false
}

// appendServiceDiag enriches a service failure event with the last lines
// of the container's crun log, captured from the guest. dew-oci-run
// writes each service's output to /var/log/dew-oci-<name>.log and removes
// the crun container on failure, so the log file is the reliable place to
// learn WHY a service didn't come up. Collection failures are ignored —
// diagnostics are best-effort.
func appendServiceDiag(ev map[string]interface{}, exec func(string) (*RunResult, error), name string) {
	if res, err := exec(services.LogTailCmd(name, 20)); err == nil && res != nil {
		out := strings.TrimSpace(res.Stdout)
		if out == "" {
			out = strings.TrimSpace(res.Stderr)
		}
		if out != "" {
			ev["logs"] = out
		}
	}
}
