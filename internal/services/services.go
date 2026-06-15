// Package services defines pre-configured service definitions
// for dew up --with.
package services

import (
	"fmt"
	"strings"
)

type Service struct {
	Name      string
	Image     string
	Port      int
	Env       []string
	DataDir   string // mount path inside container for persistence
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
