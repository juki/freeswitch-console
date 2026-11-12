//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"fsdeck/internal/license"
	"fsdeck/internal/store"
)

// keyFor signs l with a fresh key pair and returns the checked key.
func keyFor(t *testing.T, l license.License) license.Key {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	k, err := license.Sign(l, priv)
	if err != nil {
		t.Fatal(err)
	}
	return license.Check(k, pub)
}

func TestLicenseInfo(t *testing.T) {
	h := newHarness(t)
	tok := h.login("viewer", viewerPW)
	expect(t, h.do("GET", "/api/license", tok, ""), 200, `{"edition":"community","status":"none","licensee":"","plan":"","features":[],"expires":null}`)

	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	h.srv.cfg.License = keyFor(t, license.License{ID: "L1", Licensee: "Acme", Plan: "pro", Features: []string{"users", "audit"}, Issued: 1, Expires: exp.Unix()})
	expect(t, h.do("GET", "/api/license", tok, ""), 200, `{"edition":"enterprise","status":"valid","licensee":"Acme","plan":"pro","features":["users","audit"],"expires":"2030-01-02T03:04:05Z"}`)

	h.srv.cfg.License = keyFor(t, license.License{Licensee: "Acme", Plan: "pro", Features: []string{"users"}, Issued: 1, Expires: 2})
	expect(t, h.do("GET", "/api/license", tok, ""), 200, `{"edition":"community","status":"expired","licensee":"Acme","plan":"pro","features":["users"],"expires":"1970-01-01T00:00:02Z"}`)

	h.srv.cfg.License = keyFor(t, license.License{Licensee: "Forever", Features: []string{"audit"}, Issued: 1})
	expect(t, h.do("GET", "/api/license", tok, ""), 200, `{"edition":"enterprise","status":"valid","licensee":"Forever","plan":"","features":["audit"],"expires":null}`)

	h.srv.cfg.License = license.Load("garbage.key")
	expect(t, h.do("GET", "/api/license", tok, ""), 200, `{"edition":"community","status":"invalid","licensee":"","plan":"","features":[],"expires":null}`)
	expect(t, h.do("GET", "/api/license", "", ""), 401, "")
}

func TestUnlicensedAccounts(t *testing.T) {
	var auditPath string
	h := newHarness(t, noEnvAccounts, withAudit(t, &auditPath))
	h.addUser("admin", store.RoleAdmin)
	h.addUser("viewer", store.RoleViewer)
	h.addUser("carol", store.RoleViewer)
	h.addUser("ops", store.RoleOperator)

	// Wrong password: the usual answer, so the licence rule tells nothing.
	expect(t, h.loginAs("carol", "wrong-password"), 401, `{"error":"invalid username or password"}`)
	for _, name := range []string{"carol", "ops"} {
		expect(t, h.loginAs(name, userPW), 403, `{"error":"`+errUnlicensedAccount+`"}`)
	}
	admin := h.login("admin", userPW)
	h.login("viewer", userPW)
	expect(t, h.do("GET", "/api/users", admin, ""), 200, "")
	expect(t, h.do("POST", "/api/users", admin, `{"username":"dave","role":"viewer","password":"`+userPW+`"}`), 402, `{"error":"this feature needs a licence","feature":"users"}`)
	expect(t, h.do("GET", "/api/audit", admin, ""), 402, `{"error":"this feature needs a licence","feature":"audit"}`)
	data := string(must(t, auditPath))
	if !strings.Contains(data, `"user":"ops","ip":"127.0.0.1","action":"login.failed","target":"ops","detail":"not covered by the licence","result":"denied"`) {
		t.Fatalf("audit %s", data)
	}

	// A licence with only some features.
	h.srv.cfg.License = keyFor(t, license.License{Licensee: "x", Features: []string{"audit"}, Issued: 1})
	expect(t, h.do("GET", "/api/audit", admin, ""), 200, "")
	expect(t, h.do("DELETE", "/api/users/0000000000000000", admin, ""), 402, "")
	expect(t, h.loginAs("ops", userPW), 403, "")

	// With users, then expired while signed in: the session ends.
	h.srv.cfg.License = keyFor(t, license.License{Licensee: "x", Features: []string{"users"}, Issued: 1})
	ops := h.login("ops", userPW)
	expect(t, h.do("DELETE", "/api/instances/"+h.id+"/channels/abc-123", ops, ""), 200, "")
	h.srv.cfg.License = keyFor(t, license.License{Licensee: "x", Features: []string{"users"}, Issued: 1, Expires: time.Now().Add(-time.Second).Unix()})
	expect(t, h.do("GET", "/api/me", ops, ""), 401, "")
	expect(t, h.do("GET", "/api/me", admin, ""), 200, "")
}
