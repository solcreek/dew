//go:build darwin

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// resolveSSHKey picks the public key to seed into the new server's
// authorized_keys. The resolution order is tuned for both interactive
// humans and headless agents:
//
//  1. --no-ssh-key flag           — opt out, returns ("", "none", nil)
//  2. --ssh-key <value>           — flag is auto-detected:
//       - starts with "ssh-"      → treat as literal key contents
//       - equals "-"              → read stdin
//       - otherwise               → treat as a file path
//  3. DEW_SSH_KEY env var         — literal key contents (agent ideal:
//                                    set the env, no path discipline,
//                                    survives container boundaries)
//  4. DEW_SSH_KEY_FILE env var    — path
//  5. ~/.ssh/id_ed25519.pub       — auto-discovery default
//  6. ~/.ssh/id_rsa.pub           — auto-discovery fallback
//  7. nothing                     — returns ("", "none", nil); the
//                                    caller decides whether to fail
//                                    or proceed with password auth
//
// The returned source string is for stderr logging so the caller (or
// an agent reading dew's output) knows where the key came from.
func resolveSSHKey(flagValue string, noKey bool) (key, source string, err error) {
	if noKey {
		return "", "none (--no-ssh-key)", nil
	}

	if flagValue != "" {
		k, src, err := resolveSSHKeyFlag(flagValue)
		if err != nil {
			return "", "", err
		}
		return k, src, nil
	}

	if v := os.Getenv("DEW_SSH_KEY"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), "DEW_SSH_KEY env", nil
	}
	if v := os.Getenv("DEW_SSH_KEY_FILE"); v != "" {
		k, err := readSSHKeyFile(v)
		if err != nil {
			return "", "", fmt.Errorf("DEW_SSH_KEY_FILE=%s: %w", v, err)
		}
		return k, "DEW_SSH_KEY_FILE=" + v, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", "none (home dir not resolvable)", nil
	}
	for _, candidate := range []string{
		filepath.Join(home, ".ssh", "id_ed25519.pub"),
		filepath.Join(home, ".ssh", "id_rsa.pub"),
	} {
		if k, err := readSSHKeyFile(candidate); err == nil {
			return k, "auto-discovered " + candidate, nil
		}
	}

	return "", "none", nil
}

// resolveSSHKeyFlag interprets the value of --ssh-key. The form is
// auto-detected so agents don't need a separate flag per shape.
func resolveSSHKeyFlag(v string) (key, source string, err error) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "ssh-") {
		// Inline literal — most agent path. Validate shape lightly:
		// SSH public keys are three space-separated fields (type,
		// base64 blob, optional comment). Anything else is a hint
		// the user passed a malformed value.
		fields := strings.Fields(v)
		if len(fields) < 2 {
			return "", "", fmt.Errorf("--ssh-key literal must be in the form 'ssh-<type> <base64> [comment]'")
		}
		return v, "--ssh-key literal", nil
	}
	if v == "-" {
		// Stdin. Streamable from any tool. Trimmed so a trailing
		// newline doesn't bloat authorized_keys.
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "", fmt.Errorf("--ssh-key -: read stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), "--ssh-key stdin", nil
	}
	// File path. ~/foo expansion is a courtesy for the
	// human-friendly case.
	expanded := v
	if strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, expanded[2:])
		}
	}
	k, err := readSSHKeyFile(expanded)
	if err != nil {
		return "", "", fmt.Errorf("--ssh-key %s: %w", v, err)
	}
	return k, "--ssh-key " + v, nil
}

func readSSHKeyFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
