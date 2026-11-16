package server

import (
	"os"
	"testing"

	"github.com/juki/fsdeck/internal/store"
)

const userPW = "user-password-1"

// addUser stores a file account with password userPW and returns its ID.
func (h *harness) addUser(name, role string) string {
	h.t.Helper()
	hash, err := hashPassword(userPW)
	if err != nil {
		h.t.Fatal(err)
	}
	u, err := h.st.AddUser(store.User{Username: name, Role: role, PasswordHash: hash})
	if err != nil {
		h.t.Fatal(err)
	}
	return u.ID
}

// noEnvAccounts runs the server on file accounts only.
func noEnvAccounts(c *Config) { c.AdminPassword, c.ViewerPassword = "", "" }

func must(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
