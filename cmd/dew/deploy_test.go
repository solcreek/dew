//go:build darwin

package main

import (
	"os"
	"testing"
)

func TestResolveEndpoint(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1.2.3.4", "https://1.2.3.4:9080"},
		{"myserver.com", "https://myserver.com:9080"},
		{"http://localhost:9080", "http://localhost:9080"},
		{"https://api.creek.dev", "https://api.creek.dev"},
		{"https://example.com/", "https://example.com"},
	}
	for _, tt := range tests {
		got := resolveEndpoint(tt.input)
		if got != tt.want {
			t.Errorf("resolveEndpoint(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestInferAppName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-app.tar.gz", "my-app"},
		{"/tmp/demo.tar.gz", "demo"},
		{"app.tgz", "app"},
		{"project-v1.2.tar.gz", "project-v1.2"},
	}
	for _, tt := range tests {
		got := inferAppName(tt.input)
		if got != tt.want {
			t.Errorf("inferAppName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLoadDeployToken_EnvVar(t *testing.T) {
	os.Setenv("DEW_TOKEN", "test-token-123")
	defer os.Unsetenv("DEW_TOKEN")

	token, err := loadDeployToken("any-target")
	if err != nil {
		t.Fatal(err)
	}
	if token != "test-token-123" {
		t.Errorf("got %q, want test-token-123", token)
	}
}

func TestLoadDeployToken_Missing(t *testing.T) {
	os.Unsetenv("DEW_TOKEN")
	os.Unsetenv("CREEK_TOKEN")

	_, err := loadDeployToken("nonexistent-host")
	if err == nil {
		t.Error("expected error for missing token")
	}
}
