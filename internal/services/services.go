// Package services defines pre-configured service definitions
// for dew up --with.
package services

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// HostAlias is the hostname the guest and its containers resolve to the
// macOS host (the VZ NAT gateway). See initramfs/build.sh (init-stage2).
const HostAlias = "host.internal"

// hostAliasRef matches a candidate host.internal[:port] / host.dew.internal[:port]
// reference inside a service env value, e.g. ANYCABLE_RPC_HOST=host.internal:50051
// or REDIS_URL=redis://host.dew.internal:6379/0. The port group is optional so a
// bare host.internal reference is still detected (port 0, reported portless).
//
// RE2 has no lookaround, so this matches the bare token; hostname-boundary
// checks are applied in HostInternalPorts to reject substrings of a longer
// name (myhost.internal, host.internalfoo).
var hostAliasRef = regexp.MustCompile(`host\.(?:dew\.)?internal(?::(\d+))?`)

// isHostnameByte reports whether b can appear inside a DNS hostname token
// (letters, digits, '.', '-', '_'). Used to enforce that host.internal is a
// standalone hostname, not a tail/substring of a different name.
func isHostnameByte(b byte) bool {
	return b == '.' || b == '-' || b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// HostInternalPorts returns the distinct, ascending ports a service's env
// reaches on the macOS host via host.internal / host.dew.internal. A bare
// reference with no port yields a single 0 entry so callers can still warn
// about the 0.0.0.0-bind requirement without a specific port. Returns nil when
// the env names no standalone host.internal alias — including when the only
// references carry an out-of-range port.
//
// dew uses this to surface a heads-up at boot: a host service reached this way
// must bind 0.0.0.0 (a 127.0.0.1 bind is unreachable from the VM), and the
// failure is otherwise a silent connection-refused inside the container.
func HostInternalPorts(env []string) []int {
	seen := map[int]bool{}
	var ports []int
	for _, e := range env {
		for _, m := range hostAliasRef.FindAllStringSubmatchIndex(e, -1) {
			start, end := m[0], m[1]
			// Standalone-hostname boundary: reject when the token is part of a
			// longer name (myhost.internal, host.internalfoo) rather than a
			// hostname on its own. A trailing :port is inside [start,end].
			if start > 0 && isHostnameByte(e[start-1]) {
				continue
			}
			if end < len(e) && isHostnameByte(e[end]) {
				continue
			}
			port := 0
			if m[2] >= 0 { // port subgroup present
				p, err := strconv.Atoi(e[m[2]:m[3]])
				if err != nil || p < 1 || p > 65535 {
					continue
				}
				port = p
			}
			if !seen[port] {
				seen[port] = true
				ports = append(ports, port)
			}
		}
	}
	sort.Ints(ports)
	return ports
}

type Service struct {
	Name    string
	Image   string
	Port    int
	Env     []string
	DataDir string // mount path inside container for persistence
	// Args are appended after the image's entrypoint+cmd (server flags).
	// Used e.g. to force mysql to bind IPv4 so the forwarded port is
	// reachable.
	Args []string
}

var Registry = map[string]Service{
	"postgres": {
		Name:    "postgres",
		Image:   "docker.io/library/postgres:16-alpine",
		Port:    5432,
		Env:     []string{"POSTGRES_PASSWORD=dew", "POSTGRES_DB=dew"},
		DataDir: "/var/lib/postgresql/data",
	},
	"redis": {
		Name:  "redis",
		Image: "docker.io/library/redis:7-alpine",
		Port:  6379,
	},
	"mysql": {
		Name:    "mysql",
		Image:   "docker.io/library/mysql:8-oracle",
		Port:    3306,
		Env:     []string{"MYSQL_ROOT_PASSWORD=dew", "MYSQL_DATABASE=dew"},
		DataDir: "/var/lib/mysql",
		// mysql:8 defaults to binding only the IPv6 wildcard, leaving the
		// forwarded IPv4 port (127.0.0.1:3306) unreachable. Force IPv4.
		Args: []string{"--bind-address=0.0.0.0"},
	},
	"mongo": {
		Name:  "mongo",
		Image: "docker.io/library/mongo:7",
		Port:  27017,
	},
	"minio": {
		Name:  "minio",
		Image: "docker.io/minio/minio:latest",
		Port:  9000,
		Env:   []string{"MINIO_ROOT_USER=dew", "MINIO_ROOT_PASSWORD=dewpassword"},
	},
}

// ListenProbeCmd returns a guest shell command that exits 0 only when
// something is listening on the given TCP port. It reads /proc/net/tcp and
// /proc/net/tcp6 directly (no ss/nc dependency) and matches the port in hex
// against sockets in the LISTEN state (st == 0A).
//
// Both stacks are scanned because many services (anything written in Go —
// mailpit, anycable-go — plus default Node binds) listen on the dual-stack
// IPv6 wildcard [::]:port, which appears ONLY in /proc/net/tcp6. dew's port
// forward dials 127.0.0.1, and a dual-stack socket still accepts that via an
// IPv4-mapped connection, so such a service IS reachable and must count as
// ready — scanning only IPv4 produced a false "never became ready" while the
// service was serving fine. A crun container that came up but whose service
// then died has no listen socket on either stack, which is how we still catch
// a "running" container that isn't actually accepting connections.
//
// cat-then-awk (not awk with two file args) so a kernel without IPv6
// (/proc/net/tcp6 absent) degrades to scanning IPv4 alone instead of awk
// erroring on the missing file.
func ListenProbeCmd(port int) string {
	return fmt.Sprintf(
		`cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk '$2 ~ ":%04X$" && $4=="0A"{f=1} END{exit !f}'`,
		port)
}

// HostServiceHint renders the boot-time advisory for a service whose env
// reaches the macOS host via host.internal. ports comes from
// HostInternalPorts (a 0 means a portless reference). Returns "" when ports is
// empty so callers can skip silently.
func HostServiceHint(service string, ports []int) string {
	if len(ports) == 0 {
		return ""
	}
	var refs []string
	for _, p := range ports {
		if p == 0 {
			refs = append(refs, HostAlias)
		} else {
			refs = append(refs, fmt.Sprintf("%s:%d", HostAlias, p))
		}
	}
	return fmt.Sprintf(
		"%s reaches the host at %s — bind that host service to 0.0.0.0 (not 127.0.0.1) so the VM can reach it",
		service, strings.Join(refs, ", "))
}

// LogPath is where dew-oci-run writes a detached service's stdout/stderr
// in the guest.
func LogPath(name string) string {
	return "/var/log/dew-oci-" + name + ".log"
}

// LogTailCmd returns a guest command printing the last n lines of a
// service's crun log.
func LogTailCmd(name string, n int) string {
	return fmt.Sprintf("tail -n %d %s 2>/dev/null", n, LogPath(name))
}

// EnvVal returns the value of env var key from the service definition,
// or "" if unset.
func EnvVal(s Service, key string) string {
	for _, e := range s.Env {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"=")
		}
	}
	return ""
}

// ConnString returns a client connection string for service s reachable
// at host port p (over 127.0.0.1). Returns "" for services without a
// well-known URI scheme. Credentials come from the service's env
// defaults so callers no longer have to dig through /proc/*/environ to
// learn them.
func ConnString(s Service, p int) string {
	switch s.Name {
	case "postgres":
		return fmt.Sprintf("postgresql://postgres:%s@127.0.0.1:%d/%s",
			EnvVal(s, "POSTGRES_PASSWORD"), p, EnvVal(s, "POSTGRES_DB"))
	case "mysql":
		return fmt.Sprintf("mysql://root:%s@127.0.0.1:%d/%s",
			EnvVal(s, "MYSQL_ROOT_PASSWORD"), p, EnvVal(s, "MYSQL_DATABASE"))
	case "redis":
		return fmt.Sprintf("redis://127.0.0.1:%d", p)
	case "mongo":
		return fmt.Sprintf("mongodb://127.0.0.1:%d", p)
	case "minio":
		return fmt.Sprintf("http://%s:%s@127.0.0.1:%d",
			EnvVal(s, "MINIO_ROOT_USER"), EnvVal(s, "MINIO_ROOT_PASSWORD"), p)
	}
	return ""
}

// Lookup returns a service by name, or nil if not found.
func Lookup(name string) *Service {
	s, ok := Registry[name]
	if !ok {
		return nil
	}
	return &s
}

// Names returns all registered service names.
func Names() []string {
	var names []string
	for k := range Registry {
		names = append(names, k)
	}
	return names
}
