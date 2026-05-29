package validate

import "testing"

func TestAppName(t *testing.T) {
	good := []string{"excalidraw", "uptime-kuma", "my-app", "ghost"}
	for _, name := range good {
		if err := AppName(name); err != nil {
			t.Errorf("AppName(%q) should pass, got: %v", name, err)
		}
	}
	bad := []string{"", "../etc/passwd", "app?token=x", "app#hash", "app%20name", "a/b", "a b"}
	for _, name := range bad {
		if err := AppName(name); err == nil {
			t.Errorf("AppName(%q) should fail", name)
		}
	}
}

func TestTarget(t *testing.T) {
	good := []string{"1.2.3.4", "my-server.com", "http://localhost:9080", "https://api.creek.dev"}
	for _, target := range good {
		if err := Target(target); err != nil {
			t.Errorf("Target(%q) should pass, got: %v", target, err)
		}
	}
	bad := []string{"", "../../etc", "host name"}
	for _, target := range bad {
		if err := Target(target); err == nil {
			t.Errorf("Target(%q) should fail", target)
		}
	}
}

func TestEnvKey(t *testing.T) {
	good := []string{"DATABASE_URL", "PORT", "NODE_ENV"}
	for _, key := range good {
		if err := EnvKey(key); err != nil {
			t.Errorf("EnvKey(%q) should pass, got: %v", key, err)
		}
	}
	bad := []string{"", "KEY=VAL", "KEY VAL"}
	for _, key := range bad {
		if err := EnvKey(key); err == nil {
			t.Errorf("EnvKey(%q) should fail", key)
		}
	}
}

func TestPort(t *testing.T) {
	if err := Port(3000); err != nil {
		t.Error(err)
	}
	if err := Port(0); err == nil {
		t.Error("port 0 should fail")
	}
	if err := Port(99999); err == nil {
		t.Error("port 99999 should fail")
	}
}
