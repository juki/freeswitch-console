//go:build community

package server

import (
	"testing"

	"fsdeck/internal/store"
)

// A community build has no paid routes.
func paidRoutes(string) []routeCase { return nil }

func matrixRuns(*testing.T) []matrixRun { return []matrixRun{{name: "community"}} }

func TestCommunityBuild(t *testing.T) {
	h := newHarness(t, noEnvAccounts, licensed(t))
	h.addUser("admin", store.RoleAdmin)
	h.addUser("ops", store.RoleOperator)
	h.addUser("carol", store.RoleViewer)
	admin := h.login("admin", userPW)
	// A valid key changes nothing in a community build.
	expect(t, h.do("GET", "/api/license", admin, ""), 200, `{"edition":"community","status":"none","licensee":"","plan":"","features":[],"expires":null}`)
	for _, name := range []string{"ops", "carol"} {
		expect(t, h.loginAs(name, userPW), 403, `{"error":"`+errUnlicensedAccount+`"}`)
	}
	p := "/api/instances/" + h.id
	for _, rq := range [][3]string{
		{"GET", "/api/users", ""},
		{"POST", "/api/users", `{"username":"x1","role":"viewer","password":"` + userPW + `"}`},
		{"PUT", "/api/users/0000000000000000", `{}`},
		{"DELETE", "/api/users/0000000000000000", ""},
		{"GET", "/api/audit", ""},
		{"GET", p + "/files?path=", ""},
		{"GET", p + "/files/content?path=a.xml", ""},
		{"PUT", p + "/files/content", `{"path":"a.xml","content":"","baseSha256":""}`},
		{"DELETE", p + "/files/content?path=a.xml&baseSha256=x", ""},
		{"GET", p + "/files/history?path=a.xml", ""},
		{"GET", p + "/files/backup?path=a.xml&id=x", ""},
		{"POST", p + "/files/restore", `{"path":"a.xml","id":"x","baseSha256":""}`},
	} {
		expect(t, h.do(rq[0], rq[1], admin, rq[2]), 404, `{"error":"not found"}`)
	}
	if allowed(store.RoleOperator, capRead) {
		t.Fatal("operator role in a community build")
	}
}
