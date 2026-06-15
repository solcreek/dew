// Package services defines pre-configured service definitions
// for dew up --with.
package services

import (
	"fmt"
	"strings"
)

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
// something is listening on the given TCP port on the IPv4 stack. It
// reads /proc/net/tcp directly (no ss/nc dependency) and matches the
// port in hex against sockets in the LISTEN state (st == 0A).
//
// Requiring an IPv4 listen socket is deliberate: dew's port forward
// dials 127.0.0.1 (IPv4), so an IPv6-only bind would never be reachable
// and must not count as ready. A crun container that came up but whose
// service then died has no listen socket, which is how we catch a
// "running" container that isn't actually accepting connections.
func ListenProbeCmd(port int) string {
	return fmt.Sprintf(
		`awk '$2 ~ ":%04X$" && $4=="0A"{f=1} END{exit !f}' /proc/net/tcp`,
		port)
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
