// Package agentauth contains the vsock request authorization predicate
// used by dew-agent. Extracted from the agent binary so the auth
// invariant can be unit-tested on any host OS (the agent itself is
// //go:build linux only).
package agentauth

// IsAuthorized reports whether a request bearing givenToken may
// proceed. Fail-closed: returns false unless the host has both sent
// SetTokenRequest (tokenSet == true) AND the request's token matches
// the stored one. Previously the check was `tokenSet && given !=
// expected`, which allowed any request arriving BEFORE the first
// SetTokenRequest (boot race window) to bypass auth entirely.
func IsAuthorized(givenToken, expectedToken string, tokenSet bool) bool {
	return tokenSet && givenToken == expectedToken
}
