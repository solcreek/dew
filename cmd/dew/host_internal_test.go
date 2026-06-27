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
	// `default via <gw> dev ...` line).
	for _, want := range []string{"ip route", "/^default/", "$3"} {
		if !strings.Contains(script, want) {
			t.Errorf("init-stage2 no longer derives the gateway from the default route (missing %q)", want)
		}
	}
	// Both alias names must be written to /etc/hosts.
	if !strings.Contains(script, "host.internal host.dew.internal") {
		t.Error("init-stage2 no longer writes the host.internal/host.dew.internal aliases")
	}
	if !strings.Contains(script, "} > /etc/hosts") {
		t.Error("init-stage2 no longer writes the guest /etc/hosts")
	}
}

// dew-oci-run must propagate the guest's host.internal line into each
// container's /etc/hosts, so a container reaches macOS host services the same
// way the guest can. Without this, only the guest — not the crun containers
// that actually run the services — could resolve host.internal.
func TestOCIRunPropagatesHostInternal(t *testing.T) {
	script := readBuildScript(t)
	if !strings.Contains(script, "grep host.internal /etc/hosts") {
		t.Error("dew-oci-run no longer copies the host.internal line into the container /etc/hosts")
	}
}
