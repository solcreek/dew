//go:build darwin

package session

import "testing"

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	id2 := generateID()
	if len(id1) != 16 {
		t.Errorf("ID length = %d, want 16", len(id1))
	}
	if id1 == id2 {
		t.Error("generated IDs should be unique")
	}
}

func TestGenerateToken(t *testing.T) {
	tok := generateToken()
	if len(tok) != 32 {
		t.Errorf("token length = %d, want 32", len(tok))
	}
}
