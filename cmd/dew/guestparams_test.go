//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

// appendGuestParams must be called from both cmdStart and cmdRun so the
// flags affect /proc/cmdline in either entry point. cmdRun used to skip
// this, silently making --share and --network-policy no-ops.
func TestAppendGuestParams(t *testing.T) {
	cases := []struct {
		name     string
		cfg      vm.Config
		wantSubs []string
		notSubs  []string
	}{
		{
			name: "shared dirs",
			cfg: vm.Config{
				SharedDirs: []vm.SharedDir{{Tag: "wkspace"}, {Tag: "proj"}},
			},
			wantSubs: []string{"dew.share=wkspace:/wkspace", "dew.share=proj:/proj"},
		},
		{
			name: "restricted policy with allowlist",
			cfg: vm.Config{
				Network:       true,
				NetworkPolicy: "restricted",
				AllowHosts:    []string{"1.1.1.1", "8.8.8.8"},
			},
			wantSubs: []string{"dew.netpolicy=restricted", "dew.allow=1.1.1.1,8.8.8.8"},
		},
		{
			name: "restricted policy without allowlist still passes netpolicy",
			cfg: vm.Config{
				Network:       true,
				NetworkPolicy: "restricted",
			},
			wantSubs: []string{"dew.netpolicy=restricted"},
			notSubs:  []string{"dew.allow="},
		},
		{
			name: "policy ignored when network is off",
			cfg: vm.Config{
				Network:       false,
				NetworkPolicy: "restricted",
				AllowHosts:    []string{"1.1.1.1"},
			},
			notSubs: []string{"dew.netpolicy", "dew.allow"},
		},
		{
			name:    "open policy is a no-op on cmdline",
			cfg:     vm.Config{Network: true, NetworkPolicy: "open"},
			notSubs: []string{"dew.netpolicy", "dew.allow"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			appendGuestParams(&cfg)
			for _, want := range tc.wantSubs {
				if !strings.Contains(cfg.CmdLine, want) {
					t.Errorf("missing %q in cmdline: %q", want, cfg.CmdLine)
				}
			}
			for _, no := range tc.notSubs {
				if strings.Contains(cfg.CmdLine, no) {
					t.Errorf("unexpected %q in cmdline: %q", no, cfg.CmdLine)
				}
			}
		})
	}
}
