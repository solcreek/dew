// Package dewfile parses dew.toml, the optional first-class project
// descriptor for `dew up`.
//
// Auto-detection (internal/detect) stays the no-config default; dew.toml is
// the canonical, machine-portable descriptor that adds what detection can't
// express: composing several arbitrary OCI services in one VM, and pinning
// the profile / dev command instead of re-deriving them every run. When a
// dew.toml is present it augments and overrides detection; when it is
// absent nothing changes.
package dewfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/solcreek/dew/internal/services"
)

// Filename is the descriptor's fixed name in a project directory.
const Filename = "dew.toml"

// File is a parsed, validated dew.toml.
type File struct {
	Project  Project   `toml:"project"`
	Dev      Dev       `toml:"dev"`
	Host     Host      `toml:"host"`
	Services []Service `toml:"service"`
}

// Host configures access from the VM back to the macOS host.
type Host struct {
	// Expose lists macOS host ports to make reachable from inside the VM
	// over a vsock reverse-forward, surfaced in the guest as
	// host.lo.internal:<port>. Unlike host.internal (the NAT gateway, which
	// needs the host service bound to 0.0.0.0), this reaches a host service
	// bound to 127.0.0.1 and bypasses the network stack entirely.
	Expose []int `toml:"expose"`
}

// Project holds project-level settings.
type Project struct {
	Name    string `toml:"name"`
	Profile string `toml:"profile"` // minimal|node|python|standard; overrides detection
}

// Dev overrides the detected dev workflow.
type Dev struct {
	Install string `toml:"install"` // dependency install command
	Command string `toml:"command"` // dev server command
	Port    int    `toml:"port"`    // dev server port to forward
}

// Service is one OCI service to run alongside the project in the same VM.
// It mirrors services.Service so dew.toml services flow through the same
// host-pull + crun staging path as the built-in `--with` services.
type Service struct {
	Name  string   `toml:"name"`  // identifier; also the crun id and stage dir
	Image string   `toml:"image"` // OCI reference (any registry image)
	Port  int      `toml:"port"`  // primary container port: health-gated and forwarded
	Ports []string `toml:"ports"` // extra host forwards beyond port: "8025" or "1080:8025"
	Env   []string `toml:"env"`   // KEY=VALUE pairs added to the container env
	Data  string   `toml:"data"`  // container path persisted on the VM disk
	Args  []string `toml:"args"`  // extra args appended after the image entrypoint
}

// parsePortSpec parses an extra-forward spec from a [[service]] ports entry:
// "PORT" (host == container) or "HOST:CONTAINER". Both ports must be 1..65535.
func parsePortSpec(s string) (host, container int, err error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	host, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port %q", s)
	}
	container = host
	if len(parts) == 2 {
		container, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid port %q", s)
		}
	}
	if host < 1 || host > 65535 || container < 1 || container > 65535 {
		return 0, 0, fmt.Errorf("port out of range in %q", s)
	}
	return host, container, nil
}

// Path returns the dew.toml path for a project directory.
func Path(dir string) string { return filepath.Join(dir, Filename) }

// Exists reports whether dir contains a dew.toml.
func Exists(dir string) bool {
	_, err := os.Stat(Path(dir))
	return err == nil
}

// Load reads, decodes, and validates dir/dew.toml. It returns (nil, nil)
// when the file is absent so callers can treat "no descriptor" as the
// no-config path without special-casing os.IsNotExist.
func Load(dir string) (*File, error) {
	data, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Filename, err)
	}
	// Reject unknown keys so a typo (e.g. `imag = ...`) fails loudly
	// instead of silently doing nothing.
	if undec := md.Undecoded(); len(undec) > 0 {
		return nil, fmt.Errorf("%s: unknown key %q", Filename, undec[0].String())
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

var validProfiles = map[string]bool{
	"minimal": true, "node": true, "python": true, "standard": true,
}

func (f *File) validate() error {
	if p := f.Project.Profile; p != "" && !validProfiles[p] {
		return fmt.Errorf("%s: invalid profile %q (want minimal|node|python|standard)", Filename, p)
	}
	if f.Dev.Port < 0 || f.Dev.Port > 65535 {
		return fmt.Errorf("%s: dev.port %d out of range", Filename, f.Dev.Port)
	}
	for _, p := range f.Host.Expose {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%s: host.expose port %d out of range (1..65535)", Filename, p)
		}
	}
	seen := make(map[string]bool, len(f.Services))
	for i := range f.Services {
		s := &f.Services[i]
		switch {
		case s.Name == "":
			return fmt.Errorf("%s: service #%d is missing a name", Filename, i+1)
		case !validServiceName(s.Name):
			return fmt.Errorf("%s: invalid service name %q (start with a lowercase letter or digit; then lowercase letters, digits, '-' or '_')", Filename, s.Name)
		case seen[s.Name]:
			return fmt.Errorf("%s: duplicate service name %q", Filename, s.Name)
		case s.Image == "":
			return fmt.Errorf("%s: service %q is missing an image", Filename, s.Name)
		case s.Port < 1 || s.Port > 65535:
			// Every service is forwarded and health-gated on its port, so a
			// missing/zero port would silently hang the readiness probe for
			// 30s. Require it up front for a clear error instead.
			return fmt.Errorf("%s: service %q needs a port in 1..65535 (got %d)", Filename, s.Name, s.Port)
		case s.Data != "" && !strings.HasPrefix(s.Data, "/"):
			// data is the OCI bind-mount destination inside the container,
			// which must be an absolute path.
			return fmt.Errorf("%s: service %q data path %q must be absolute", Filename, s.Name, s.Data)
		}
		seen[s.Name] = true
		for _, e := range s.Env {
			if !strings.Contains(e, "=") {
				return fmt.Errorf("%s: service %q env %q must be KEY=VALUE", Filename, s.Name, e)
			}
		}
		// Each forward must request a distinct host port. The primary `port`
		// already binds host port s.Port, so a `ports` entry reusing it (or
		// another entry's host port) would trip the daemon's busy-port fallback
		// and silently bind some other free port instead of the one asked for.
		// Reject up-front so the surprising remap can't happen.
		seenHost := map[int]bool{s.Port: true}
		for _, p := range s.Ports {
			h, _, err := parsePortSpec(p)
			if err != nil {
				return fmt.Errorf("%s: service %q ports: %v (want \"PORT\" or \"HOST:CONTAINER\", ports 1..65535)", Filename, s.Name, err)
			}
			if seenHost[h] {
				if h == s.Port {
					return fmt.Errorf("%s: service %q ports entry %q reuses the service's primary host port %d", Filename, s.Name, p, h)
				}
				return fmt.Errorf("%s: service %q ports has duplicate host port %d", Filename, s.Name, h)
			}
			seenHost[h] = true
		}
	}
	return nil
}

// validServiceName guards the name before it becomes a stage dir, an
// overlay path component (/var/lib/dew/oci/<name>), and a crun container
// id. Stricter than VM names: lowercase letters and digits only, with
// '-'/'_' allowed except as the first character.
func validServiceName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// ServiceList returns the descriptor's services as services.Service values,
// ready for the same staging + launch path as the built-in `--with`
// services.
func (f *File) ServiceList() []services.Service {
	out := make([]services.Service, 0, len(f.Services))
	for _, s := range f.Services {
		var extra []services.ExtraForward
		for _, p := range s.Ports {
			// validate() already rejected malformed specs, so a parse error
			// here can't happen for a loaded File; skip defensively rather
			// than panic if ServiceList is ever called on an unvalidated File.
			if h, c, err := parsePortSpec(p); err == nil {
				extra = append(extra, services.ExtraForward{Host: h, Container: c})
			}
		}
		out = append(out, services.Service{
			Name:    s.Name,
			Image:   s.Image,
			Port:    s.Port,
			Env:     s.Env,
			DataDir: s.Data,
			Args:    s.Args,
			Extra:   extra,
		})
	}
	return out
}
