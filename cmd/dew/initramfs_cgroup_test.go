//go:build darwin

package main

import (
	"strings"
	"testing"
)

// The guest init must honor the dew.* cgroup cmdline tokens emitted by
// appendGuestParams (see TestAppendGuestParams_Cgroup). These string
// guards keep init-stage2 in sync with the host so a renamed token or a
// dropped apply step is caught without booting a VM.
func TestInitramfsBuildScript_CgroupApply(t *testing.T) {
	script := readBuildScript(t)

	for _, want := range []string{
		"dew.cpu_quota=*)",
		"dew.mem_limit=*)",
		"dew.pids_max=*)", // R4 added this token; init must parse it
	} {
		if !strings.Contains(script, want) {
			t.Errorf("init-stage2 does not parse cmdline token %q", want)
		}
	}

	// Limits on the /dew leaf are inert unless the root delegates the
	// controllers down — this is the step the field notes had to do by hand.
	if !strings.Contains(script, "cgroup.subtree_control") {
		t.Error("init-stage2 never enables controllers via cgroup.subtree_control; leaf memory.max/pids.max/cpu.max would not exist")
	}
	if !strings.Contains(script, "/sys/fs/cgroup/dew/pids.max") {
		t.Error("init-stage2 never writes pids.max on the dew leaf")
	}

	// The cap only contains the workload if the agent (and its exec
	// children) actually live in the dew cgroup.
	if !strings.Contains(script, "/sys/fs/cgroup/dew/cgroup.procs") {
		t.Error("init-stage2 never moves the agent into the dew cgroup; --cgroup limits would not contain the workload")
	}

	// The move MUST use `sh -c` (an invoked shell's $$ is its own PID, kept
	// across exec), NOT a `( … )` subshell — in POSIX/BusyBox ash $$ inside a
	// subshell is the PARENT (PID 1), so a subshell writes init into the leaf
	// and leaves the agent uncapped. Guard the exact regression.
	if strings.Contains(script, "( echo $$ > /sys/fs/cgroup/dew/cgroup.procs") {
		t.Error("agent placement uses a ( … ) subshell: $$ expands to PID 1, not the agent — limits won't contain the workload and init gets moved into the leaf")
	}
	if !strings.Contains(script, `sh -c "echo \$\$ > /sys/fs/cgroup/dew/cgroup.procs`) {
		t.Error("agent placement must use `sh -c` so $$ is the agent's own PID (preserved across exec)")
	}
}
