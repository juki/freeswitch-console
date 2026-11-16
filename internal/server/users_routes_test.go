package server

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juki/fsdeck/internal/store"
)

func TestManagedAccounts(t *testing.T) {
	h := newHarness(t)
	// A file account named like a managed one is hidden and cannot sign in.
	hidden := h.addUser("admin", store.RoleViewer)
	expect(t, h.loginAs("admin", userPW), 401, "")
	admin := h.login("admin", adminPW)
	expect(t, h.do("GET", "/api/me", admin, ""), 200, `{"username":"admin","role":"admin","managed":true}`)

	alice := h.addUser("alice", store.RoleOperator)
	r := h.do("GET", "/api/users", admin, "")
	var list []map[string]any
	r.json(t, &list)
	if len(list) != 3 || list[0]["id"] != managedAdminID || list[1]["id"] != managedViewerID || list[2]["id"] != alice {
		t.Fatalf("list %s", r.body)
	}
	if list[0]["managed"] != true || list[0]["createdAt"] != nil || list[2]["managed"] != false || list[2]["createdAt"] == nil {
		t.Fatalf("list %s", r.body)
	}
	if !strings.HasPrefix(r.body, `[{"id":"env-admin","username":"admin","role":"admin","disabled":false,"managed":true,"createdAt":null},{"id":"env-viewer","username":"viewer","role":"viewer","disabled":false,"managed":true,"createdAt":null},{"id":"`+alice+`","username":"alice","role":"operator","disabled":false,"managed":false,"createdAt":"`) {
		t.Fatalf("shape %s", r.body)
	}
	if strings.Contains(r.body, "pbkdf2") || strings.Contains(r.body, "passwordHash") {
		t.Fatalf("hash exposed: %s", r.body)
	}

	for _, id := range []string{managedAdminID, managedViewerID} {
		expect(t, h.do("PUT", "/api/users/"+id, admin, `{"role":"operator"}`), 403, `{"error":"`+errManaged+`"}`)
		expect(t, h.do("PUT", "/api/users/"+id, admin, `{"password":"a-new-password"}`), 403, "")
		expect(t, h.do("DELETE", "/api/users/"+id, admin, ""), 403, "")
	}
	expect(t, h.do("PUT", "/api/users/"+hidden, admin, `{"role":"admin"}`), 404, `{"error":"user not found"}`)
	expect(t, h.do("DELETE", "/api/users/"+hidden, admin, ""), 404, "")
	expect(t, h.do("POST", "/api/users", admin, `{"username":"viewer","role":"viewer","password":"`+userPW+`"}`), 409, `{"error":"username already exists"}`)
	expect(t, h.do("POST", "/api/me/password", admin, `{"current":"`+adminPW+`","new":"another-password"}`), 403, `{"error":"`+errManaged+`"}`)
	h.login("admin", adminPW)
	h.login("viewer", viewerPW)
}

func TestUserCRUD(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)

	r := h.do("POST", "/api/users", admin, `{"username":"bob.ops","role":"operator","password":"`+userPW+`"}`)
	expect(t, r, 201, "")
	var bob userView
	r.json(t, &bob)
	if len(bob.ID) != 16 || bob.Username != "bob.ops" || bob.Role != "operator" || bob.Disabled || bob.Managed || bob.CreatedAt == nil || time.Since(*bob.CreatedAt) > time.Minute {
		t.Fatalf("created %s", r.body)
	}
	if strings.Contains(r.body, "pbkdf2") || strings.Contains(r.body, userPW) {
		t.Fatalf("leak %s", r.body)
	}
	expect(t, h.do("POST", "/api/users", admin, `{"username":"bob.ops","role":"viewer","password":"`+userPW+`"}`), 409, `{"error":"username already exists"}`)
	for _, body := range []string{
		`{"username":"B","role":"viewer","password":"` + userPW + `"}`,
		`{"username":"has space","role":"viewer","password":"` + userPW + `"}`,
		`{"username":"carol","role":"root","password":"` + userPW + `"}`,
		`{"username":"carol","role":"","password":"` + userPW + `"}`,
		`{"username":"carol","role":"viewer","password":"short"}`,
		`{"username":"carol","role":"viewer","password":"` + strings.Repeat("x", 257) + `"}`,
		`{"username":"carol","role":"viewer"}`,
		`{"username":"carol","role":"viewer","password":"` + userPW + `","disabled":true}`,
		`{"username":"carol","role":"viewer","password":"` + userPW + `","passwordHash":"x"}`,
	} {
		expect(t, h.do("POST", "/api/users", admin, body), 400, "")
	}
	if _, ok := h.st.UserByName("carol"); ok {
		t.Fatal("invalid user stored")
	}

	bobTok := h.login("bob.ops", userPW)
	expect(t, h.do("GET", "/api/me", bobTok, ""), 200, `{"username":"bob.ops","role":"operator","managed":false}`)

	// No change keeps the sessions; an empty password keeps the password.
	r = h.do("PUT", "/api/users/"+bob.ID, admin, `{}`)
	expect(t, r, 200, "")
	expect(t, h.do("PUT", "/api/users/"+bob.ID, admin, `{"role":"operator","disabled":false,"password":""}`), 200, "")
	expect(t, h.do("GET", "/api/me", bobTok, ""), 200, "")

	// A role change ends the sessions; the next sign-in has the new role.
	r = h.do("PUT", "/api/users/"+bob.ID, admin, `{"role":"viewer"}`)
	expect(t, r, 200, "")
	var upd userView
	r.json(t, &upd)
	if upd.Role != "viewer" || upd.ID != bob.ID || !upd.CreatedAt.Equal(*bob.CreatedAt) {
		t.Fatalf("updated %s", r.body)
	}
	expect(t, h.do("GET", "/api/me", bobTok, ""), 401, "")
	bobTok = h.login("bob.ops", userPW)
	expect(t, h.do("GET", "/api/me", bobTok, ""), 200, `{"username":"bob.ops","role":"viewer","managed":false}`)

	// Disabled: sessions end and sign-in fails like a wrong password.
	expect(t, h.do("PUT", "/api/users/"+bob.ID, admin, `{"disabled":true}`), 200, "")
	expect(t, h.do("GET", "/api/me", bobTok, ""), 401, "")
	expect(t, h.loginAs("bob.ops", userPW), 401, `{"error":"invalid username or password"}`)
	expect(t, h.do("PUT", "/api/users/"+bob.ID, admin, `{"disabled":false}`), 200, "")
	bobTok = h.login("bob.ops", userPW)

	// A new password ends the sessions and replaces the old one.
	expect(t, h.do("PUT", "/api/users/"+bob.ID, admin, `{"password":"bobs-new-password"}`), 200, "")
	expect(t, h.do("GET", "/api/me", bobTok, ""), 401, "")
	expect(t, h.loginAs("bob.ops", userPW), 401, "")
	bobTok = h.login("bob.ops", "bobs-new-password")

	for _, body := range []string{`{"role":"root"}`, `{"password":"short"}`, `{"disabled":"yes"}`, `{"username":"x1"}`} {
		expect(t, h.do("PUT", "/api/users/"+bob.ID, admin, body), 400, "")
	}
	expect(t, h.do("PUT", "/api/users/0000000000000000", admin, `{}`), 404, `{"error":"user not found"}`)

	expect(t, h.do("DELETE", "/api/users/"+bob.ID, admin, ""), 204, "")
	expect(t, h.do("GET", "/api/me", bobTok, ""), 401, "")
	expect(t, h.loginAs("bob.ops", "bobs-new-password"), 401, "")
	expect(t, h.do("DELETE", "/api/users/"+bob.ID, admin, ""), 404, "")

	// A recreated account of the same name does not inherit sessions.
	h.addUser("bob.ops", store.RoleAdmin)
	expect(t, h.do("GET", "/api/me", bobTok, ""), 401, "")
}

func TestAdminGuards(t *testing.T) {
	h := newHarness(t, noEnvAccounts)
	root := h.addUser("root", store.RoleAdmin)
	ops := h.addUser("ops", store.RoleAdmin)
	rootTok := h.login("root", userPW)

	// Self: no delete, disable or demote; other changes are fine.
	for _, rq := range [][2]string{{"PUT", `{"disabled":true}`}, {"PUT", `{"role":"operator"}`}, {"PUT", `{"role":"viewer","disabled":true}`}, {"DELETE", ""}} {
		expect(t, h.do(rq[0], "/api/users/"+root, rootTok, rq[1]), 409, `{"error":"you cannot delete, disable or demote the account you are signed in with"}`)
	}
	expect(t, h.do("PUT", "/api/users/"+root, rootTok, `{"role":"admin","disabled":false}`), 200, "")
	if u, _ := h.st.User(root); u.Role != store.RoleAdmin || u.Disabled {
		t.Fatalf("root changed: %+v", u)
	}

	// Changing one's own password keeps this session and ends the others.
	other := h.login("root", userPW)
	expect(t, h.do("PUT", "/api/users/"+root, rootTok, `{"password":"roots-new-password"}`), 200, "")
	expect(t, h.do("GET", "/api/me", rootTok, ""), 200, "")
	expect(t, h.do("GET", "/api/me", other, ""), 401, "")

	// Another admin can be disabled while one stays enabled.
	expect(t, h.do("PUT", "/api/users/"+ops, rootTok, `{"disabled":true}`), 200, "")
	expect(t, h.do("PUT", "/api/users/"+ops, rootTok, `{"disabled":false}`), 200, "")
	opsTok := h.login("ops", userPW)
	expect(t, h.do("PUT", "/api/users/"+root, opsTok, `{"role":"operator"}`), 200, "")
	expect(t, h.do("GET", "/api/me", rootTok, ""), 401, "")

	// ops is now the last enabled admin. Requests that passed the role
	// check before the demotion landed hit the last-admin guard.
	h.srv.usersMu.Lock()
	cur, _ := h.st.User(ops)
	msg := h.srv.guardAdmin(user{ID: root}, cur, false)
	h.srv.usersMu.Unlock()
	if msg != "the last enabled admin cannot be deleted, disabled or demoted" {
		t.Fatalf("guard %q", msg)
	}
	// A disabled admin does not count.
	h.addUser("spare", store.RoleAdmin)
	spare, _ := h.st.UserByName("spare")
	expect(t, h.do("PUT", "/api/users/"+spare.ID, opsTok, `{"disabled":true}`), 200, "")
	if msg := h.srv.guardAdmin(user{ID: root}, cur, false); msg == "" {
		t.Fatal("disabled admin counted")
	}
	// Demoting a non-admin or a disabled admin needs no guard.
	expect(t, h.do("PUT", "/api/users/"+spare.ID, opsTok, `{"role":"viewer"}`), 200, "")
	expect(t, h.do("DELETE", "/api/users/"+root, opsTok, ""), 204, "")
}

// TestAdminGuardsConcurrent has two admins demote each other at the same
// time, over and over: one admin always stays.
func TestAdminGuardsConcurrent(t *testing.T) {
	h := newHarness(t, noEnvAccounts)
	a := h.addUser("aa", store.RoleAdmin)
	b := h.addUser("bb", store.RoleAdmin)
	for round := 0; round < 20; round++ {
		ta, tb := h.login("aa", userPW), h.login("bb", userPW)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, rq := range [][2]string{{ta, b}, {tb, a}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = h.do("PUT", "/api/users/"+rq[1], rq[0], `{"role":"viewer"}`).code
			}()
		}
		wg.Wait()
		if h.srv.enabledAdmins() != 1 {
			t.Fatalf("round %d: %d enabled admins, codes %v", round, h.srv.enabledAdmins(), codes)
		}
		sort.Ints(codes)
		if codes[0] != 200 || (codes[1] != 409 && codes[1] != 401 && codes[1] != 403) {
			t.Fatalf("round %d: codes %v", round, codes)
		}
		for _, id := range []string{a, b} {
			u, _ := h.st.User(id)
			u.Role = store.RoleAdmin
			h.st.UpdateUser(u)
		}
	}
}

func TestLogsNeedOperator(t *testing.T) {
	h := newHarness(t, noEnvAccounts)
	f := h.fake(0)
	for _, role := range []string{store.RoleViewer, store.RoleOperator, store.RoleAdmin} {
		h.addUser("u-"+role, role)
		tok := h.login("u-"+role, userPW)
		ch, cancel := h.openStream(tok, "?loglevel=7")
		next(t, ch)
		f.publish(logMsg("3", "line for "+role))
		f.publish(heartbeat)
		ev := next(t, ch)
		if allowed(role, capLogs) {
			if ev.name != "backlog" {
				t.Fatalf("%s: %+v", role, ev)
			}
			if ev := next(t, ch); ev.name != "log" {
				t.Fatalf("%s: %+v", role, ev)
			}
		} else if ev.name != "heartbeat" {
			t.Fatalf("%s got %+v", role, ev)
		}
		cancel()
	}
}
