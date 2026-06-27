//go:build darwin

package main

import (
	"strings"
	"testing"
)

// init-stage2 must publish the macOS host as host.internal / host.dew.internal
// in the guest /etc/hosts, resolving to the VZ NAT gateway (the default
// route's nexthop). This is dew's host.docker.internal: it lets guest dev
// servers and containers reach host services without hardcoding 192.168.64.1,
// which VZ may renumber. Drift here silently strands every host-callback.
func TestInitStage2PublishesHostInternal(t *testing.T) {
	script := readBuildScript(t)

	// The gateway is derived from the default route's nexthop ($3 of the
	// `default via <gw> dev ...` line). The match is pinned to `via` so a
	// vialess default route (`default dev eth0`) can't capture the interface
	// name as a bogus gateway IP.
	for _, want := range []string{"ip route", `$1=="default" && $2=="via"`, "$3"} {
		if !strings.Contains(script, want) {
			t.Errorf("init-stage2 no longer derives the gateway from the default-via route (missing %q)", want)
		}
	}
	// Both alias names must be written to /etc/hosts.
	if !strings.Contains(script, "host.internal host.dew.internal") {
		t.Error("init-stage2 no longer writes the host.internal/host.dew.internal aliases")
	}
	if !strings.Contains(script, "} > /etc/hosts") {
		t.Error("init-stage2 no longer writes the guest /etc/hosts")
	}
	// host.lo.internal → 127.0.0.2 is the reverse host-forward alias the agent
	// listens on for `dew up --expose-host`.
	if !strings.Contains(script, "127.0.0.2\\thost.lo.internal") {
		t.Error("init-stage2 no longer writes the host.lo.internal (127.0.0.2) reverse-forward alias")
	}
}

// dew-oci-run must propagate the guest's host alias lines into each container's
// /etc/hosts, so a container reaches macOS host services the same way the guest
// can — both host.internal (NAT gateway) and host.lo.internal (reverse-forward).
// `grep -F .internal` matches every alias line and no localhost line.
func TestOCIRunPropagatesHostAliases(t *testing.T) {
	script := readBuildScript(t)
	if !strings.Contains(script, "grep -F .internal /etc/hosts") {
		t.Error("dew-oci-run no longer copies the host alias lines into the container /etc/hosts")
	}
}
