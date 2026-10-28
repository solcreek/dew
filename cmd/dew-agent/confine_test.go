//go:build linux

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	protocol "github.com/solcreek/dew/internal/vsock"
)

func envValues(env []string, key string) []string {
	var out []string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// The shim takes its spec and its fallback identity from the environment, so
// the parent must make both authoritative: a request's own DEW_CONFINE_SPEC
// or DEW_CONFINE_EXEC_USER must never reach the shim. The fallback identity
// is the agent's DEW_EXEC_USER, which before this was read by the shim from
// the merged env, where a request's DEW_EXEC_USER could override it.
func TestConfineExecCmdEnvIsAuthoritative(t *testing.T) {
	saved := execUser
	t.Cleanup(func() { execUser = saved })

	spec := &protocol.Confinement{User: "65534", ReadOnlyRoot: true}
	req := protocol.ExecRequest{
		Command: "/bin/true",
		Env: []string{
			"FOO=bar",
			"DEW_CONFINE_SPEC={}",
			"DEW_CONFINE_EXEC_USER=0",
		},
		Confine: spec,
	}

	execUser = "dew"
	cmd := confineExecCmd(context.Background(), req)
	specs := envValues(cmd.Env, "DEW_CONFINE_SPEC")
	if len(specs) != 1 {
		t.Fatalf("DEW_CONFINE_SPEC entries = %q, want exactly the agent's", specs)
	}
	var got protocol.Confinement
	if err := json.Unmarshal([]byte(specs[0]), &got); err != nil || got.User != "65534" || !got.ReadOnlyRoot {
		t.Errorf("spec = %q (%v), want the request's Confinement", specs[0], err)
	}
	if users := envValues(cmd.Env, "DEW_CONFINE_EXEC_USER"); len(users) != 1 || users[0] != "dew" {
		t.Errorf("DEW_CONFINE_EXEC_USER entries = %q, want exactly the agent's [dew]", users)
	}
	if foo := envValues(cmd.Env, "FOO"); len(foo) != 1 || foo[0] != "bar" {
		t.Errorf("other request env lost: FOO = %q", foo)
	}

	execUser = ""
	cmd = confineExecCmd(context.Background(), req)
	if users := envValues(cmd.Env, "DEW_CONFINE_EXEC_USER"); len(users) != 0 {
		t.Errorf("no agent exec user, yet DEW_CONFINE_EXEC_USER = %q (the request's value leaked)", users)
	}
}
