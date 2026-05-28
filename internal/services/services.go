// Package services defines pre-configured service definitions
// for dew up --with.
package services

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

// NerdctlRunCmd builds the nerdctl run command for a service.
func NerdctlRunCmd(s Service) string {
	cmd := "nerdctl run -d --net=host --name " + s.Name
	for _, e := range s.Env {
		cmd += " -e " + e
	}
	cmd += " " + s.Image
	return cmd
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
