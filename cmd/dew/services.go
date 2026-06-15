//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/internal/services"
	"github.com/solcreek/dew/pkg/dewerr"
)

// serviceRow is one line of `dew services` output.
type serviceRow struct {
	Name      string `json:"name"`
	Running   bool   `json:"running"`
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Conn      string `json:"conn"`
}

// cmdServices lists the predefined services and their client connection
// strings, annotating which are currently running (forwarded) and on
// what host port. This replaces the previous need to dig through
// /proc/*/environ to recover credentials. Works with or without a
// running VM — without one, it's a static catalog.
func cmdServices(args []string) error {
	wantJSON := flagJSON
	for _, a := range args {
		if a == "--json" {
			wantJSON = true
		}
	}

	// Best-effort: learn which services are actually forwarded and on
	// what host port (which may differ from the default after a busy-
	// port fallback). A missing daemon just means "nothing running".
	hostPortByGuest := map[int]int{}
	if resp, err := sendDaemonRequest(daemon.ExecRequest{Kind: "forward-list"}); err == nil {
		if fwds, ok := resp["forwards"].([]any); ok {
			for _, f := range fwds {
				m, _ := f.(map[string]any)
				gp, _ := m["GuestPort"].(float64)
				hp, _ := m["HostPort"].(float64)
				if gp > 0 {
					hostPortByGuest[int(gp)] = int(hp)
				}
			}
		}
	}

	names := services.Names()
	sort.Strings(names)
	rows := make([]serviceRow, 0, len(names))
	for _, n := range names {
		s := services.Registry[n]
		port := s.Port
		hp, running := hostPortByGuest[s.Port]
		if running {
			port = hp
		}
		rows = append(rows, serviceRow{
			Name:      n,
			Running:   running,
			HostPort:  port,
			GuestPort: s.Port,
			Conn:      services.ConnString(s, port),
		})
	}

	if wantJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data":           map[string]any{"services": rows},
		})
	}

	fmt.Println("SERVICE   STATUS                         CONNECTION")
	for _, r := range rows {
		status := "available"
		if r.Running {
			status = fmt.Sprintf("running → 127.0.0.1:%d", r.HostPort)
		}
		fmt.Printf("%-9s %-30s %s\n", r.Name, status, r.Conn)
	}
	fmt.Println("\nStart with: dew up --with <name>[,<name>...]")
	return nil
}

// cmdLogs prints a --with service's container logs. Services run via
// crun (dew-oci-run), which writes each container's stdout/stderr to
// /var/log/dew-oci-<name>.log in the guest. This wrapper saves users
// from having to know that path or that services run under crun.
func cmdLogs(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return dewerr.New(dewerr.CodeUsage, "usage: dew logs <service>")
	}
	name := args[0]
	logPath := "/var/log/dew-oci-" + name + ".log"
	return cmdExec([]string{"cat " + shellQuote(logPath)})
}
