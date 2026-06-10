// Package vmstate persists the lifecycle phase of the VM owned by a
// dew process, so `dew status` can report transient states (booting,
// ephemeral run in flight) that the daemon socket alone cannot show:
// the socket only exists after the token handshake, and `dew run`
// never creates one at all.
//
// One JSON file in the state dir, last writer wins. The owning process
// records its PID; readers treat a file whose PID is no longer alive
// as stale (crash leftovers), so no cleanup discipline is required of
// writers beyond best-effort Clear on exit.
package vmstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Phase is the coarse lifecycle stage of the VM.
type Phase string

const (
	// PhaseBooting covers everything from VM start until the guest
	// agent answers (or the serial console reports ready).
	PhaseBooting Phase = "booting"
	// PhaseRunning means the guest is reachable: exec is possible.
	PhaseRunning Phase = "running"
)

// State is what the owning dew process publishes.
type State struct {
	PID       int       `json:"pid"`
	Phase     Phase     `json:"phase"`
	Mode      string    `json:"mode"` // "run" | "start" | "up"
	Profile   string    `json:"profile,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

const fileName = "vm-state.json"

// Path returns the state file location inside dir (the dew state dir,
// the same one that holds the daemon socket).
func Path(dir string) string {
	return filepath.Join(dir, fileName)
}

// Write publishes s atomically (temp file + rename) so a concurrent
// reader never sees a partial JSON document.
func Write(dir string, s State) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := Path(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir))
}

// Read returns the published state and whether a file was present and
// parseable. Liveness of the owning process is the caller's question —
// see Alive.
func Read(dir string) (State, bool) {
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		return State{}, false
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, false
	}
	return s, true
}

// Clear removes the state file, but only if it is still owned by pid —
// a second dew process may have already overwritten it with its own
// state, and exiting must not erase someone else's record.
func Clear(dir string, pid int) {
	s, ok := Read(dir)
	if !ok || s.PID != pid {
		return
	}
	os.Remove(Path(dir))
}

// Alive reports whether pid refers to a live process. Signal 0 probes
// without delivering anything; EPERM still means the process exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
