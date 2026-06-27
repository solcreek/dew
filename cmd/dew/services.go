//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/internal/dewfile"
	"github.com/solcreek/dew/internal/services"
	"github.com/solcreek/dew/pkg/dewerr"
)

// serviceCatalog returns the services `dew services` lists: the built-in
// registry plus any dew.toml [[service]] entries in dir (a custom image with a
// registry name overrides the built-in), sorted by name. df-absent dirs yield
// just the built-ins, preserving the static-catalog behaviour. This is why a
// running mailpit/anycable from dew.toml now shows up, not only the five
// managed services.
//
// A broken dew.toml (parse/validation error) is surfaced, not swallowed:
// silently falling back to the built-ins would honour neither dewfile.Load's
// "fail loudly" contract nor the user, who'd see a catalog that omits their
// declared services with no hint why. Only (nil, nil) — no dew.toml present —
// is the clean built-ins-only case.
func serviceCatalog(dir string) ([]services.Service, error) {
	byName := make(map[string]services.Service, len(services.Registry))
	for name, s := range services.Registry {
		byName[name] = s
	}
	df, err := dewfile.Load(dir)
	if err != nil {
		return nil, err
	}
	if df != nil {
		for _, s := range df.ServiceList() {
			byName[s.Name] = s
		}
	}
	out := make([]services.Service, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// serviceRow is one line of `dew services` output.
type serviceRow struct {
	Name      string `json:"name"`
	Running   bool   `json:"running"`
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Conn      string `json:"conn"`
}

// pickPrimaryHostPort reduces guest→[]host (from forward-list) to one host
// port per guest port, deterministically. Multiple forwards can share a guest
// port when a dew.toml `ports` entry remaps a second host port onto a
// service's primary container port (e.g. "1080:8025" alongside port 8025), and
// forward-list order is unspecified because the daemon stores forwards in a
// map. The forward whose host port equals the guest port wins — a service's
// primary forward defaults to host==guest — otherwise the smallest host port.
// Without this `dew services` could nondeterministically report an extra's
// host port (and connection string) for the main service row.
func pickPrimaryHostPort(hostPortsByGuest map[int][]int) map[int]int {
	out := make(map[int]int, len(hostPortsByGuest))
	for gp, hps := range hostPortsByGuest {
		pick := -1
		for _, hp := range hps {
			if hp == gp { // a primary forward's default host==guest mapping
				pick = hp
				break
			}
			if pick == -1 || hp < pick {
				pick = hp
			}
		}
		if pick >= 0 {
			out[gp] = pick
		}
	}
	return out
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
	// A guest port can carry more than one forward when a dew.toml `ports`
	// entry remaps a second host port onto a service's primary container
	// port, so collect all host ports per guest and reduce deterministically.
	hostPortsByGuest := map[int][]int{}
	if resp, err := sendDaemonRequest(daemon.ExecRequest{Kind: "forward-list"}); err == nil {
		if fwds, ok := resp["forwards"].([]any); ok {
			for _, f := range fwds {
				m, _ := f.(map[string]any)
				gp, _ := m["GuestPort"].(float64)
				hp, _ := m["HostPort"].(float64)
				// Require a real host port: a failed type assertion leaves hp at
				// 0, and since pickPrimaryHostPort takes the smallest, a stray 0
				// would win and misreport the service as running on port 0.
				if gp > 0 && hp > 0 {
					hostPortsByGuest[int(gp)] = append(hostPortsByGuest[int(gp)], int(hp))
				}
			}
		}
	}
	hostPortByGuest := pickPrimaryHostPort(hostPortsByGuest)

	catalog, err := serviceCatalog(".")
	if err != nil {
		return err
	}
	rows := make([]serviceRow, 0, len(catalog))
	for _, s := range catalog {
		port := s.Port
		hp, running := hostPortByGuest[s.Port]
		if running {
			port = hp
		}
		// Built-in services have a known URI scheme; a custom dew.toml image
		// (mailpit, anycable) doesn't, so fall back to a plain host:port so
		// the row still carries a copy-pasteable address.
		conn := services.ConnString(s, port)
		if conn == "" {
			conn = fmt.Sprintf("127.0.0.1:%d", port)
		}
		rows = append(rows, serviceRow{
			Name:      s.Name,
			Running:   running,
			HostPort:  port,
			GuestPort: s.Port,
			Conn:      conn,
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
