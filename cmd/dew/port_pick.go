//go:build darwin

package main

import (
	"fmt"
	"net"

	"github.com/solcreek/dew/internal/vm"
)

// pickFreeHostPort returns an available host TCP port starting at
// preferred and incrementing up to `attempts` candidates. Probes
// IPv4 + IPv6 (loopback and wildcard) to catch processes bound on
// either family — the same dual-stack defense grove uses, ported
// here for `dew up` so a dev server collision doesn't fall over.
//
// Returns (chosen, didSubstitute, error). didSubstitute is true
// when chosen != preferred; callers surface that to the user.
//
// Mirrors grove's install.FindFreeHostPort. Lives here as a copy
// because dew and grove are independent repos with no shared module
// — duplicating ~50 lines beats a third "solcreek/portutil" repo
// for now. If a third caller (marina) wants the same logic, that's
// the tipping point to extract.
func pickFreeHostPort(preferred, attempts int) (int, bool, error) {
	if preferred < 1 || preferred > 65535 {
		return 0, false, fmt.Errorf("preferred port %d out of range", preferred)
	}
	if attempts < 1 {
		attempts = 1
	}
	for offset := 0; offset < attempts; offset++ {
		p := preferred + offset
		if p > 65535 {
			break
		}
		if portFreeForBind(p) {
			return p, p != preferred, nil
		}
	}
	return 0, false, fmt.Errorf("no free host port in range %d-%d", preferred, preferred+attempts-1)
}

// provisionalForward builds the initial PortForward entry used at
// VM-boot time for `dew up`. Probes for a free host port starting
// at the framework's default; if all 50 candidates are taken the
// caller's bind will still fail loudly, but in the common case the
// shift is invisible. The guest port stays at the framework default
// since dew can't know yet which port the dev server will actually
// bind to (vite.config.ts may override) — runtime detection in
// cmdUp's launch loop will add an additional forward if it differs.
func provisionalForward(guestPort int) vm.PortForward {
	host, _, err := pickFreeHostPort(guestPort, 50)
	if err != nil {
		// Fall back to the requested port; the daemon's AddForward
		// will surface the real bind error.
		host = guestPort
	}
	return vm.PortForward{HostPort: host, GuestPort: guestPort}
}

// portFreeForBind: try to bind all four common targets (IPv4 +
// IPv6, loopback + wildcard). True ONLY when every one succeeds —
// guards against the case where a process holds *:N IPv6 (e.g.
// `bun dev`) but our explicit 127.0.0.1:N would succeed despite
// the conflict at localhost-DNS resolution time.
func portFreeForBind(p int) bool {
	for _, addr := range []string{
		fmt.Sprintf("127.0.0.1:%d", p),
		fmt.Sprintf("0.0.0.0:%d", p),
		fmt.Sprintf("[::1]:%d", p),
		fmt.Sprintf("[::]:%d", p),
	} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		ln.Close()
	}
	return true
}
