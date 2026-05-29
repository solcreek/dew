// Package validate provides input hardening for agent-safety.
// Agents hallucinate paths, embed query params in IDs, and generate
// control characters. Validate at the boundary.
package validate

import (
	"fmt"
	"strings"
)

func AppName(name string) error {
	if name == "" {
		return fmt.Errorf("app name is required")
	}
	if len(name) > 128 {
		return fmt.Errorf("app name too long (max 128 chars)")
	}
	for _, c := range name {
		if c < 0x20 {
			return fmt.Errorf("app name contains control character")
		}
	}
	for _, bad := range []string{"/", "..", "?", "#", "%", " ", "\t"} {
		if strings.Contains(name, bad) {
			return fmt.Errorf("app name contains invalid character %q", bad)
		}
	}
	return nil
}

func Target(target string) error {
	if target == "" {
		return fmt.Errorf("target is required")
	}
	for _, c := range target {
		if c < 0x20 {
			return fmt.Errorf("target contains control character")
		}
	}
	if strings.Contains(target, "..") {
		return fmt.Errorf("target contains path traversal")
	}
	if strings.Contains(target, " ") {
		return fmt.Errorf("target contains space")
	}
	return nil
}

func EnvKey(key string) error {
	if key == "" {
		return fmt.Errorf("env key is required")
	}
	for _, c := range key {
		if c < 0x20 {
			return fmt.Errorf("env key contains control character")
		}
		if c == '=' || c == ' ' || c == '\t' {
			return fmt.Errorf("env key contains invalid character")
		}
	}
	return nil
}

func Port(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be 1-65535, got %d", port)
	}
	return nil
}
