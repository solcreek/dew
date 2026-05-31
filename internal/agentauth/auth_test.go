package agentauth

import "testing"

func TestIsAuthorized(t *testing.T) {
	cases := []struct {
		name     string
		tokenSet bool
		expected string
		given    string
		want     bool
	}{
		{"token not set, no given — reject (was: accept, race window)", false, "", "", false},
		{"token not set, attacker guesses — reject", false, "", "guess", false},
		{"token set, no given — reject", true, "secret", "", false},
		{"token set, mismatch — reject", true, "secret", "wrong", false},
		{"token set, match — allow", true, "secret", "secret", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsAuthorized(tc.given, tc.expected, tc.tokenSet)
			if got != tc.want {
				t.Errorf("IsAuthorized(given=%q, expected=%q, tokenSet=%v) = %v, want %v",
					tc.given, tc.expected, tc.tokenSet, got, tc.want)
			}
		})
	}
}
