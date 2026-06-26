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
	Services []Service `toml:"service"`
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
	Port  int      `toml:"port"`  // container port to health-gate and forward
	Env   []string `toml:"env"`   // KEY=VALUE pairs added to the container env
	Data  string   `toml:"data"`  // container path persisted on the VM disk
	Args  []string `toml:"args"`  // extra args appended after the image entrypoint
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
	seen := make(map[string]bool, len(f.Services))
	for i := range f.Services {
		s := &f.Services[i]
		switch {
		case s.Name == "":
			return fmt.Errorf("%s: service #%d is missing a name", Filename, i+1)
		case !validServiceName(s.Name):
			return fmt.Errorf("%s: invalid service name %q (use lowercase letters, digits, '-' or '_')", Filename, s.Name)
		case seen[s.Name]:
			return fmt.Errorf("%s: duplicate service name %q", Filename, s.Name)
		case s.Image == "":
			return fmt.Errorf("%s: service %q is missing an image", Filename, s.Name)
		case s.Port < 0 || s.Port > 65535:
			return fmt.Errorf("%s: service %q port %d out of range", Filename, s.Name, s.Port)
		}
		seen[s.Name] = true
		for _, e := range s.Env {
			if !strings.Contains(e, "=") {
				return fmt.Errorf("%s: service %q env %q must be KEY=VALUE", Filename, s.Name, e)
			}
		}
	}
	return nil
}

// validServiceName guards the name before it becomes a stage dir, an
// overlay path component (/var/lib/dew/oci/<name>), and a crun container
// id. Mirrors the conservative charset used for VM names.
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
		out = append(out, services.Service{
			Name:    s.Name,
			Image:   s.Image,
			Port:    s.Port,
			Env:     s.Env,
			DataDir: s.Data,
			Args:    s.Args,
		})
	}
	return out
}
