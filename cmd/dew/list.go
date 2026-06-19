//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/internal/vmstate"
)

// vmListEntry is one row of `dew vm list` — the same facts `dew vm
// status` reports, for every VM the state dir knows about.
type vmListEntry struct {
	Name      string `json:"name"`
	Running   bool   `json:"running"`
	Phase     string `json:"phase,omitempty"`
	Profile   string `json:"profile,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Socket    string `json:"socket"`
	StartedAt string `json:"started_at,omitempty"`
}

// cmdList enumerates every VM with a daemon socket or a live state file —
// the default (unnamed) VM and any named VMs started with --name. It is
// the discovery half of named VMs: a front door routing `ssh <user>` to
// a VM needs to know which names exist. Read-only and always exit 0.
func cmdList(args []string) error {
	for _, a := range args {
		if a == "--json" {
			flagJSON = true
		}
	}

	base := daemon.SocketDir()
	entries := make([]vmListEntry, 0)
	for _, name := range collectVMNames(base) {
		sock := daemon.SocketPath(name)

		running := false
		if _, err := os.Stat(sock); err == nil {
			if c, derr := net.DialTimeout("unix", sock, 300*time.Millisecond); derr == nil {
				c.Close()
				running = true
			}
		}

		e := vmListEntry{Name: displayVMName(name), Running: running, Socket: sock}
		if st, ok := vmstate.Read(vmstate.DirFor(base, name)); ok && vmstate.Alive(st.PID) {
			e.Phase = string(st.Phase)
			e.Profile = st.Profile
			e.PID = st.PID
			e.StartedAt = st.StartedAt.Format(time.RFC3339)
		}
		// A name with neither a live socket nor live state is a crash
		// leftover (stale socket file / dead state) — don't list it.
		if !running && e.PID == 0 {
			continue
		}
		entries = append(entries, e)
	}

	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data":           map[string]any{"vms": entries},
		})
	}

	if len(entries) == 0 {
		fmt.Println("dew: no VMs running")
		fmt.Println("  Start: dew vm start --profile standard [--name <vm>]")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tPROFILE\tPID\tUP")
	for _, e := range entries {
		status := "stopped"
		switch {
		case e.Running:
			status = "running"
		case e.Phase == string(vmstate.PhaseBooting):
			status = "booting"
		case e.Phase == string(vmstate.PhaseRunning):
			status = "running*" // reachable guest, no daemon socket (ephemeral)
		}
		up, profile, pid := "-", "-", "-"
		if e.Profile != "" {
			profile = e.Profile
		}
		if e.PID != 0 {
			pid = fmt.Sprintf("%d", e.PID)
		}
		if e.StartedAt != "" {
			if t, err := time.Parse(time.RFC3339, e.StartedAt); err == nil {
				up = time.Since(t).Round(time.Second).String()
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Name, status, profile, pid, up)
	}
	return tw.Flush()
}

// displayVMName renders the empty (unnamed) VM as "default" for humans
// and JSON consumers; named VMs are shown verbatim.
func displayVMName(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

// collectVMNames discovers VM names from the state dir: <name>.sock
// files (default.sock → the unnamed VM), the base vm-state.json (a
// booting/ephemeral default VM), and <name>/vm-state.json subdirs. The
// empty string represents the default VM and sorts first.
func collectVMNames(base string) []string {
	set := map[string]struct{}{}
	dirents, _ := os.ReadDir(base)
	for _, de := range dirents {
		n := de.Name()
		if de.IsDir() {
			if _, err := os.Stat(vmstate.Path(filepath.Join(base, n))); err == nil {
				set[n] = struct{}{}
			}
			continue
		}
		switch {
		case n == "vm-state.json":
			set[""] = struct{}{}
		case strings.HasSuffix(n, ".sock"):
			s := strings.TrimSuffix(n, ".sock")
			if s == "default" {
				set[""] = struct{}{}
			} else {
				set[s] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out) // "" (default) sorts before any named VM
	return out
}
