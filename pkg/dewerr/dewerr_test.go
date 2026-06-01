package dewerr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCodeSlug_Stable(t *testing.T) {
	// Slugs are part of the public contract; this test fails if anyone
	// renames one in a way that would break downstream agents.
	expected := map[Code]string{
		CodeGeneric:     "error",
		CodeUsage:       "usage",
		CodeAuth:        "auth",
		CodeNetwork:     "network",
		CodeNotFound:    "not_found",
		CodeConflict:    "conflict",
		CodeTimeout:     "timeout",
		CodeUnavailable: "unavailable",
	}
	for c, want := range expected {
		if got := c.Slug(); got != want {
			t.Errorf("Code(%d).Slug() = %q, want %q", c, got, want)
		}
	}
}

func TestCodeRetryableByDefault(t *testing.T) {
	retryable := []Code{CodeNetwork, CodeTimeout, CodeUnavailable}
	notRetryable := []Code{CodeGeneric, CodeUsage, CodeAuth, CodeNotFound, CodeConflict}
	for _, c := range retryable {
		if !c.RetryableByDefault() {
			t.Errorf("Code(%d).RetryableByDefault() = false, want true", c)
		}
	}
	for _, c := range notRetryable {
		if c.RetryableByDefault() {
			t.Errorf("Code(%d).RetryableByDefault() = true, want false", c)
		}
	}
}

func TestNew_RetryableDefault(t *testing.T) {
	if !New(CodeNetwork, "x").Retryable {
		t.Error("New(CodeNetwork) should default Retryable=true")
	}
	if New(CodeAuth, "x").Retryable {
		t.Error("New(CodeAuth) should default Retryable=false")
	}
}

func TestNewf(t *testing.T) {
	err := Newf(CodeNotFound, "app %s not running", "ghost")
	if err.Message != "app ghost not running" {
		t.Errorf("Message = %q", err.Message)
	}
	if err.Code != CodeNotFound {
		t.Errorf("Code = %v", err.Code)
	}
}

func TestWrap_PreservesInnerCode(t *testing.T) {
	// Multi-layer wrapping must not flatten the classification — a
	// network error wrapped in a "deploy failed" string is still a
	// network error from the agent's perspective.
	inner := New(CodeNetwork, "connection refused")
	outer := Wrap(inner, CodeGeneric, "deploy")
	if outer.Code != CodeNetwork {
		t.Errorf("outer.Code = %v, want CodeNetwork (preserved from inner)", outer.Code)
	}
	if !outer.Retryable {
		t.Error("outer.Retryable should be true (preserved from inner)")
	}
}

func TestWrap_AddsCodeToUntypedError(t *testing.T) {
	raw := errors.New("dial tcp: connection refused")
	wrapped := Wrap(raw, CodeNetwork, "talking to upstream")
	if wrapped.Code != CodeNetwork {
		t.Errorf("Code = %v, want CodeNetwork", wrapped.Code)
	}
	if wrapped.Cause != raw {
		t.Error("Cause should be the original error")
	}
}

func TestWrap_NilErr(t *testing.T) {
	if Wrap(nil, CodeNetwork, "x") != nil {
		t.Error("Wrap(nil, ...) should return nil")
	}
}

func TestError_Format(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{"no cause", New(CodeAuth, "token expired"), "token expired"},
		{"with cause", Wrap(errors.New("inner"), CodeNetwork, "outer"), "outer: inner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodeOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Code
	}{
		{"nil", nil, 0},
		{"untyped", errors.New("anything"), CodeGeneric},
		{"typed", New(CodeAuth, "x"), CodeAuth},
		{"typed via Wrap", Wrap(errors.New("inner"), CodeNotFound, "outer"), CodeNotFound},
		{"typed buried under fmt.Errorf %w", fmt.Errorf("outer: %w", New(CodeNetwork, "inner")), CodeNetwork},
		{"typed wrapped twice via fmt.Errorf", fmt.Errorf("a: %w", fmt.Errorf("b: %w", New(CodeTimeout, "c"))), CodeTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeOf(tc.err); got != tc.want {
				t.Errorf("CodeOf() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	if Retryable(nil) {
		t.Error("Retryable(nil) = true, want false")
	}
	if Retryable(errors.New("untyped")) {
		t.Error("untyped errors should default to non-retryable")
	}
	if !Retryable(New(CodeNetwork, "x")) {
		t.Error("CodeNetwork should be retryable")
	}
	// Caller-overridden retryable=false on a code that's retryable by default.
	custom := New(CodeNetwork, "x")
	custom.Retryable = false
	if Retryable(custom) {
		t.Error("explicit Retryable=false should win over the default")
	}
}

func TestError_FieldsRoundtrip(t *testing.T) {
	// Smoke check that Hint + RetryAfter survive Wrap with same-code.
	hint := map[string]string{"field": "image", "reason": "not_found"}
	inner := &Error{
		Code:       CodeNotFound,
		Message:    "image not found",
		Hint:       hint,
		RetryAfter: 0,
	}
	outer := Wrap(inner, CodeNotFound, "pull")
	// Wrap preserves the inner's code but copies forward the hint /
	// retry-after so callers reading the chain top don't have to walk.
	if outer.Code != CodeNotFound {
		t.Errorf("outer.Code = %v, want CodeNotFound", outer.Code)
	}
	if _, ok := outer.Hint["field"]; !ok {
		t.Errorf("outer.Hint missing fields; got %v", outer.Hint)
	}
}

// Regression guard for the contract: nobody should add codes outside
// 0–119 silently, because 120–127 is timeout(1) + chroot territory
// and 128+ is the POSIX signal range. The test enumerates the
// currently allocated codes to lock the table.
func TestCodes_InAllowedRange(t *testing.T) {
	allocated := []Code{
		CodeGeneric,
		CodeUsage,
		CodeAuth,
		CodeNetwork,
		CodeNotFound,
		CodeConflict,
		CodeTimeout,
		CodeUnavailable,
	}
	for _, c := range allocated {
		switch {
		case c == 0:
			t.Errorf("code 0 must be reserved for success; got allocated code %d", c)
		case c == 1, c == 2:
			// generic/usage — fine
		case c >= 100 && c <= 119:
			// dew-classified range — fine
		case c >= 120 && c <= 127:
			t.Errorf("code %d falls in the timeout(1)/chroot reserved range (120–127); pick 100–119", c)
		case c >= 128:
			t.Errorf("code %d falls in the POSIX signal range (128+); pick 100–119", c)
		case c >= 3 && c <= 99:
			t.Errorf("code %d collides with potential subprocess passthrough range (3–99); pick 100–119", c)
		}
	}
	_ = time.Now // touch time import so future changes can use it without re-adding
}
