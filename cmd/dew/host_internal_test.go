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

// dew-agent must not wait on the DHCP lease: it rides vsock, OCI images are
// host-pulled, port forwards are vsock, and co-located services talk over
// localhost, so the guest NIC isn't on the boot critical path. init-stage2
// therefore backgrounds bring_up_network (lease + host.internal + egress policy)
// in the default case and starts the agent without blocking on it. A restricted
// egress policy is the exception — the OUTPUT DROP must be in force before
// anything can egress — so there the bring-up stays synchronous.
func TestInitStage2BackgroundsNetworkOffCriticalPath(t *testing.T) {
	script := readBuildScript(t)

	for _, want := range []string{
		"bring_up_network()", // the network bring-up is a function
		"apply_netpolicy()",  // egress policy factored out so it can run in either path
		"bring_up_network &", // backgrounded in the default case
		"NET_PID=$!",         // its PID is captured for the later wait barriers
	} {
		if !strings.Contains(script, want) {
			t.Errorf("init-stage2 no longer takes DHCP off the critical path (missing %q)", want)
		}
	}

	// Restricted policy keeps the bring-up synchronous and preserves the
	// default-DROP egress barrier.
	if !strings.Contains(script, `if [ "$NETPOLICY" = "restricted" ]; then`) {
		t.Error("init-stage2 lost the restricted-policy synchronous branch")
	}
	if !strings.Contains(script, "OUTPUT  DROP") {
		t.Error("init-stage2 lost the restricted-policy OUTPUT DROP barrier")
	}

	// The agent must start AFTER the backgrounded bring-up, so it never waits
	// on DHCP. Anchor on the boot-time start guard (unique to init-stage2), not
	// the earlier build-time agent copy elsewhere in build.sh.
	bg := strings.Index(script, "bring_up_network &")
	agent := strings.Index(script, "[ -x /usr/local/bin/dew-agent ] && [ -e /dev/vsock ]")
	if bg < 0 || agent < 0 || agent < bg {
		t.Errorf("dew-agent boot start (idx %d) must come after the backgrounded bring_up_network (idx %d)", agent, bg)
	}

	// Network-dependent boot steps wait on the lease before proceeding.
	if !strings.Contains(script, `wait "$NET_PID"`) {
		t.Error("init-stage2 no longer waits on NET_PID before the network-dependent boot steps")
	}
}

// Backgrounding the lease races host.internal into /etc/hosts against container
// launches: dew-oci-run snapshots the guest's .internal lines per container, so
// a container started before the append would permanently miss host.internal.
// A /run/dew-net-pending marker closes the race — set when the lease is
// backgrounded, cleared once bring_up_network finishes, and waited on by
// dew-oci-run before it snapshots /etc/hosts.
func TestOCIRunWaitsForHostInternalBeforeSnapshot(t *testing.T) {
	script := readBuildScript(t)

	if !strings.Contains(script, ": > /run/dew-net-pending") {
		t.Error("init-stage2 no longer marks the backgrounded lease in flight (/run/dew-net-pending)")
	}
	if !strings.Contains(script, "rm -f /run/dew-net-pending") {
		t.Error("bring_up_network no longer clears the /run/dew-net-pending marker")
	}

	// dew-oci-run must wait on the marker, and that wait must come BEFORE it
	// snapshots the guest's .internal lines — otherwise the wait wouldn't
	// protect the snapshot it's meant to guard.
	wait := strings.Index(script, "[ -e /run/dew-net-pending ]")
	snapshot := strings.Index(script, "grep -F .internal /etc/hosts")
	if wait < 0 {
		t.Error("dew-oci-run no longer waits for the host.internal lease marker before snapshotting /etc/hosts")
	}
	if snapshot < 0 {
		t.Fatal("dew-oci-run no longer snapshots the .internal host lines")
	}
	if wait > snapshot {
		t.Errorf("dew-oci-run waits on the lease marker (idx %d) after the /etc/hosts snapshot (idx %d); it must wait first", wait, snapshot)
	}
}
