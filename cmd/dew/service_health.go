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
				o.failReason = "service did not start accepting connections within ~30s"
				o.failLogs = diag(s.name)
			}
			outcomes[i] = o
		}(i)
	}
	wg.Wait()
	return outcomes
}

// pollFloor is the tight initial cadence the readiness probe starts at before
// backing off to its steady interval (see pollBackoff). A service's listen
// socket usually binds early in the budget, so the first polls fire ~10ms
// apart to catch the bind within a few ms of it happening instead of up to a
// full steady interval late; a genuinely slow start backs off to the steady
// cap to avoid busy-spinning. Only the cadence within the (unchanged) budget
// changes.
//
// The token handshake deliberately does NOT use this: its poll is an expensive
// vsock connect against a not-yet-listening agent, so tightening early polls
// there just burns more failed connects with no payoff (measured net-negative
// for `dew run`). That wait instead uses connectVsockDeadline's own tight
// internal retry — see cmdRun / sendToken.
const pollFloor = 10 * time.Millisecond

// pollBackoff yields successive poll sleeps starting at min(floor, steady) and
// doubling up to steady. Used by waitGuestReady so a service whose port binds
// early is detected promptly rather than waiting out a flat interval. Construct
// with newPollBackoff; call next() once per sleep.
type pollBackoff struct {
	cur, steady time.Duration
}

// newPollBackoff starts the cadence at min(floor, steady) — so a steady
// interval below the floor (as fast unit tests pass) never sleeps longer than
// it asked for — and doubles toward steady.
func newPollBackoff(floor, steady time.Duration) *pollBackoff {
	start := steady
	if floor < start {
		start = floor
	}
	return &pollBackoff{cur: start, steady: steady}
}

// next returns the duration to sleep before the next poll, then advances
// (doubling, capped at steady).
func (b *pollBackoff) next() time.Duration {
	d := b.cur
	b.cur *= 2
	if b.cur > b.steady {
		b.cur = b.steady
	}
	return d
}

// Readiness polling cadence shared by `dew up` and `dew run`. A container
// typically binds its port in well under a second, so poll at a fine interval
// rather than a flat 1s — under the old 1s a service ready at ~180ms still
// waited up to a full second to be detected (and that cost was paid per
// service when they were started serially). The first polls are tighter still
// (pollFloor, backing off to readyProbeInterval) so an early bind is caught
// near-instantly. readyProbeAttempts keeps a ~30s overall budget for a
// genuinely slow first start (cold image, DB init).
const (
	readyProbeInterval = 100 * time.Millisecond
	readyProbeAttempts = 300
	// readyProbeExecTimeout bounds each individual listen-probe exec. The
	// overall ~30s budget is enforced by waitGuestReady's wall-clock deadline;
	// this bound just caps a single hung probe so the gate can overshoot the
	// deadline by at most one exec (≤5s) rather than inheriting the agent
	// default (up to 30s per attempt) and stalling startup.
	readyProbeExecTimeout = 5 * time.Second
	// serviceDiagExecTimeout bounds the best-effort log-tail collected when a
	// service fails. The concurrent launcher waits for every service goroutine,
	// so an unbounded diag exec on a slow guest would delay all results — cap it
	// rather than inherit the agent default.
	serviceDiagExecTimeout = 5 * time.Second
)

// waitGuestReady calls probe repeatedly until it returns true, the attempt
// budget is exhausted, or the wall-clock deadline (attempts*interval) passes,
// sleeping interval between tries. probe is injected (rather than calling the
// guest directly) so the readiness polling is unit-testable without a running
// VM. Returns true once the service is confirmed ready, false on timeout.
//
// The deadline matters because a probe exec is not free: it can take up to
// readyProbeExecTimeout each, so counting attempts alone could stretch the gate
// far past attempts*interval. Bounding wall-clock keeps it honest with the
// "within ~30s" the caller reports; the worst overshoot is one in-flight probe.
//
// Sleeps follow a pollBackoff (pollFloor → interval) rather than a flat
// interval: a service that binds its port early is detected within ~pollFloor
// of doing so instead of up to a full interval later, trimming the common
// fast-start case without changing the overall budget (attempts*interval).
func waitGuestReady(probe func() bool, attempts int, interval time.Duration) bool {
	deadline := time.Now().Add(time.Duration(attempts) * interval)
	backoff := newPollBackoff(pollFloor, interval)
	for i := 0; i < attempts; i++ {
		if probe() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		if i < attempts-1 {
			time.Sleep(backoff.next())
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
