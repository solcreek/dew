//go:build darwin

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDoctorSummaryCounts(t *testing.T) {
	r := DoctorReport{
		Checks: []DoctorCheck{
			{Name: "a", Status: CheckPass},
			{Name: "b", Status: CheckPass},
			{Name: "c", Status: CheckWarn},
			{Name: "d", Status: CheckFail},
		},
	}
	// Recompute summary the same way runDoctorChecks would.
	for _, c := range r.Checks {
		switch c.Status {
		case CheckPass:
			r.Summary.Pass++
		case CheckWarn:
			r.Summary.Warn++
		case CheckFail:
			r.Summary.Fail++
		}
	}
	r.OK = r.Summary.Fail == 0
	if r.Summary.Pass != 2 || r.Summary.Warn != 1 || r.Summary.Fail != 1 {
		t.Errorf("got pass=%d warn=%d fail=%d, want 2/1/1",
			r.Summary.Pass, r.Summary.Warn, r.Summary.Fail)
	}
	if r.OK {
		t.Error("OK should be false when there is a failure")
	}
}

func TestDoctorReportJSONShape(t *testing.T) {
	r := DoctorReport{
		OK: false,
		Checks: []DoctorCheck{
			{Name: "macOS version", Status: CheckPass, Value: "15.6.1"},
			{Name: "Ad-hoc", Status: CheckWarn, Code: "ad_hoc_entitlement", Message: "..."},
			{Name: "Boot", Status: CheckFail, Code: "boot_failed", Message: "VZError"},
		},
		Summary: DoctorSummary{Pass: 1, Warn: 1, Fail: 1},
	}
	buf, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}

	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	if _, ok := got["checks"]; !ok {
		t.Error("missing 'checks' field")
	}
	summary, ok := got["summary"].(map[string]any)
	if !ok {
		t.Fatal("summary not an object")
	}
	if summary["pass"].(float64) != 1 {
		t.Errorf("summary.pass = %v", summary["pass"])
	}
}

func TestDoctorCheckJSONOmitEmpty(t *testing.T) {
	// Pass check should not include error fields.
	c := DoctorCheck{Name: "x", Status: CheckPass}
	buf, _ := json.Marshal(c)
	s := string(buf)
	if strings.Contains(s, "code") || strings.Contains(s, "message") || strings.Contains(s, "hint") {
		t.Errorf("expected omitempty fields absent, got: %s", s)
	}
}

func TestPrintIndented(t *testing.T) {
	// Smoke test: shouldn't panic on multi-line input.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic: %v", r)
		}
	}()
	printIndented("line1\nline2\n\nline4")
}

func TestDoctorProfiles_OverrideWins(t *testing.T) {
	got := doctorProfiles("node")
	want := []string{"minimal", "node"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDoctorProfiles_OverrideMinimalNotDuplicated(t *testing.T) {
	got := doctorProfiles("minimal")
	if len(got) != 1 || got[0] != "minimal" {
		t.Fatalf("got %v, want [minimal]", got)
	}
}

func TestDoctorProfiles_AlwaysIncludesMinimal(t *testing.T) {
	got := doctorProfiles("python")
	if got[0] != "minimal" {
		t.Fatalf("minimal must be checked first, got %v", got)
	}
}

func TestCmdDoctor_ProfileRequiresValue(t *testing.T) {
	// --profile with no value, or followed by another flag, must error
	// during arg parse (before any checks run) rather than silently
	// falling back to auto-detection.
	for _, args := range [][]string{
		{"--profile"},
		{"--profile", "--verbose"},
	} {
		if err := cmdDoctor(args); err == nil {
			t.Errorf("cmdDoctor(%v): expected error, got nil", args)
		}
	}
}

func TestCmdDoctor_RejectsUnknownProfile(t *testing.T) {
	// An unsupported profile must fail at parse with a clear error,
	// not proceed to report a confusing missing initramfs-typo asset.
	if err := cmdDoctor([]string{"--profile", "typo"}); err == nil {
		t.Fatal("cmdDoctor --profile typo: expected error, got nil")
	}
}

func TestRunDoctorChecksSmokeRuns(t *testing.T) {
	// Smoke test: should produce a report without panicking on the host.
	// We don't assert specific check counts because they depend on the
	// environment (codesign, assets, etc.).
	r := runDoctorChecks(false, "")
	if len(r.Checks) == 0 {
		t.Error("expected at least one check")
	}
	// Summary should sum to len(checks)
	total := r.Summary.Pass + r.Summary.Warn + r.Summary.Fail
	if total != len(r.Checks) {
		t.Errorf("summary total %d != %d checks", total, len(r.Checks))
	}
	// OK is consistent with Fail count
	if r.OK != (r.Summary.Fail == 0) {
		t.Error("OK / Fail inconsistency")
	}
}
