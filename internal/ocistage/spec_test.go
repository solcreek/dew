package ocistage

import (
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestOCISpec_ArgsEnvAndHostNetworking(t *testing.T) {
	cfg := v1.Config{
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"--flag"},
		Env:        []string{"FOO=bar"},
		WorkingDir: "/work",
	}
	spec := ociSpec(cfg, GuestRootPath("svc"), nil, []string{"EXTRA=1"}, nil)

	proc := spec["process"].(map[string]any)
	args := proc["args"].([]string)
	if len(args) != 2 || args[0] != "/entry" || args[1] != "--flag" {
		t.Fatalf("args = %v, want [/entry --flag]", args)
	}
	if proc["cwd"] != "/work" {
		t.Fatalf("cwd = %v, want /work", proc["cwd"])
	}
	env := proc["env"].([]string)
	if env[0] != "FOO=bar" || env[len(env)-1] != "EXTRA=1" {
		t.Fatalf("env = %v, want image env then EXTRA=1", env)
	}

	root := spec["root"].(map[string]any)
	if root["path"] != "/var/lib/dew/oci/svc/merged" {
		t.Fatalf("root.path = %v", root["path"])
	}

	// Host networking: there must be NO network namespace.
	ns := spec["linux"].(map[string]any)["namespaces"].([]map[string]any)
	for _, n := range ns {
		if n["type"] == "network" {
			t.Fatal("spec must not declare a network namespace (host networking)")
		}
	}
}

func TestOCISpec_CmdOverrideAndBind(t *testing.T) {
	cfg := v1.Config{Entrypoint: []string{"/orig"}}
	bind := &Bind{Source: "/data/host", Destination: "/var/lib/x"}
	spec := ociSpec(cfg, "/r", []string{"echo", "hi"}, nil, bind)

	args := spec["process"].(map[string]any)["args"].([]string)
	if len(args) != 2 || args[0] != "echo" || args[1] != "hi" {
		t.Fatalf("override args = %v, want [echo hi]", args)
	}

	mounts := spec["mounts"].([]map[string]any)
	var found bool
	for _, m := range mounts {
		if m["destination"] == "/var/lib/x" && m["source"] == "/data/host" && m["type"] == "bind" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected bind mount /data/host -> /var/lib/x, mounts = %v", mounts)
	}
}

func TestOCISpec_DefaultArgsAndEnv(t *testing.T) {
	spec := ociSpec(v1.Config{}, "/r", nil, nil, nil)
	proc := spec["process"].(map[string]any)
	if got := proc["args"].([]string); len(got) != 1 || got[0] != "/bin/sh" {
		t.Fatalf("default args = %v, want [/bin/sh]", got)
	}
	if got := proc["env"].([]string); len(got) == 0 {
		t.Fatal("expected a default PATH env when image declares none")
	}
}
