//go:build darwin

package main

import (
	"strings"
	"sync"
	"time"

	"github.com/solcreek/dew/internal/services"
)

// stagedOutcome is the result of bringing up one staged service: whether its
// container launched, whether it became ready (bound its port), and, on
// failure, the reason plus a tail of the container log for diagnostics.
type stagedOutcome struct {
	launched   bool
	ready      bool
	failReason string
	failLogs   string
}

// bringUpStaged launches every staged service concurrently and returns their
// outcomes in svcs order. Each service's container launch and readiness probe
// are independent, so the wall-clock is the slowest service rather than the
// sum of all of them. launch, probeReady, and diag are injected so the
// orchestration (concurrency + ordered outcome mapping) is unit-testable
// without a VM. The caller owns the cheap, user-visible follow-up — port
// forwarding, events, spinner, stderr — and does it serially after this
// returns, so output and forward registration stay deterministic.
func bringUpStaged(
	svcs []stagedService,
	launch func(stagedService) error,
	probeReady func(stagedService) bool,
	diag func(name string) string,
) []stagedOutcome {
	outcomes := make([]stagedOutcome, len(svcs))
	var wg sync.WaitGroup
	for i := range svcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := svcs[i]
			if err := launch(s); err != nil {
				outcomes[i] = stagedOutcome{failReason: err.Error(), failLogs: diag(s.name)}
				return
			}
			o := stagedOutcome{launched: true}
			if probeReady(s) {
				o.ready = true
			} else {
				o.failReason = "service did not start accepting connections within 30s"
				o.failLogs = diag(s.name)
			}
			outcomes[i] = o
		}(i)
	}
	wg.Wait()
	return outcomes
}

// Readiness polling cadence shared by `dew up` and `dew run`. A container
// typically binds its port in well under a second, so poll at a fine interval
// rather than a flat 1s — under the old 1s a service ready at ~180ms still
// waited up to a full second to be detected (and that cost was paid per
// service when they were started serially). readyProbeAttempts keeps a ~30s
// overall budget for a genuinely slow first start (cold image, DB init).
const (
	readyProbeInterval = 100 * time.Millisecond
	readyProbeAttempts = 300
	// readyProbeExecTimeout bounds each individual listen-probe exec so the
	// overall gate stays ~readyProbeAttempts*readyProbeInterval. Without it a
	// probe exec inherits the agent default (up to 30s per attempt), and 300
	// such attempts could blow far past the intended ~30s budget.
	readyProbeExecTimeout = 5 * time.Second
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
	if logs := serviceDiag(exec, name); logs != "" {
		ev["logs"] = logs
	}
}

// serviceDiag returns the last lines of a service's crun log from the guest,
// or "" if collection failed (diagnostics are best-effort). Shared by the
// event-map enricher and the concurrent launcher's per-outcome diagnostics.
func serviceDiag(exec func(string) (*RunResult, error), name string) string {
	res, err := exec(services.LogTailCmd(name, 20))
	if err != nil || res == nil {
		return ""
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = strings.TrimSpace(res.Stderr)
	}
	return out
}
