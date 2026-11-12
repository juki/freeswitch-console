package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"fsdeck/internal/license"
	"fsdeck/internal/store"
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

// licensed gives the harness a valid licence for features (every feature
// when none is named), signed with a key made for the test.
func licensed(t *testing.T, features ...string) func(*Config) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(features) == 0 {
		features = license.Features
	}
	now := time.Now()
	key, err := license.Sign(license.License{ID: "test", Licensee: "Test Ltd", Plan: "test", Features: features, Issued: now.Unix(), Expires: now.Add(time.Hour).Unix()}, priv)
	if err != nil {
		t.Fatal(err)
	}
	return func(c *Config) { c.License = license.Check(key, pub) }
}

func must(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
