package server

import (
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

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

func TestPasswordHash(t *testing.T) {
	if defaultIterations != 600000 {
		t.Fatalf("default iterations %d", defaultIterations)
	}
	a, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hashPassword("correct horse")
	format := regexp.MustCompile(`^pbkdf2-sha256\$1000\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	if !format.MatchString(a) || a == b {
		t.Fatalf("hashes %q %q", a, b)
	}
	if !verifyPassword(a, "correct horse") || !verifyPassword(b, "correct horse") || verifyPassword(a, "correct horsE") || verifyPassword(a, "") {
		t.Fatal("verify")
	}

	// Interoperability: PBKDF2-HMAC-SHA256("password", "salt", 1, 32), the
	// RFC 7914 section 11 vector, in our format.
	key, _ := hex.DecodeString("120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b")
	vector := "pbkdf2-sha256$1$" + base64.RawStdEncoding.EncodeToString([]byte("salt")) + "$" + base64.RawStdEncoding.EncodeToString(key)
	if !verifyPassword(vector, "password") || verifyPassword(vector, "passwore") {
		t.Fatal("RFC vector")
	}
	// The count comes from the hash, not from the current setting.
	pbkdf2Iterations = 2000
	c, _ := hashPassword("x")
	pbkdf2Iterations = 1000
	if !strings.HasPrefix(c, "pbkdf2-sha256$2000$") || !verifyPassword(c, "x") {
		t.Fatalf("other count %q", c)
	}

	parts := strings.Split(a, "$")
	for _, bad := range []string{
		"",
		"plain",
		"pbkdf2-sha1$1000$" + parts[2] + "$" + parts[3],
		"pbkdf2-sha256$0$" + parts[2] + "$" + parts[3],
		"pbkdf2-sha256$-5$" + parts[2] + "$" + parts[3],
		"pbkdf2-sha256$10000001$" + parts[2] + "$" + parts[3],
		"pbkdf2-sha256$x$" + parts[2] + "$" + parts[3],
		"pbkdf2-sha256$1000$!!$" + parts[3],
		"pbkdf2-sha256$1000$" + parts[2] + "$",
		"pbkdf2-sha256$1000$" + parts[2] + "$" + parts[3] + "$x",
		"pbkdf2-sha256$1000$" + parts[2],
	} {
		if verifyPassword(bad, "correct horse") {
			t.Errorf("accepted %q", bad)
		}
	}

	for pw, ok := range map[string]bool{
		"":                       false,
		"123456789":              false,
		"1234567890":             true,
		"ééééééééé":              false, // 9 runes, 18 bytes
		"éééééééééé":             true,
		strings.Repeat("x", 256): true,
		strings.Repeat("x", 257): false,
	} {
		if (checkPassword(pw) == nil) != ok {
			t.Errorf("checkPassword(%q) = %v", pw, checkPassword(pw))
		}
	}
}

// TestUnknownUserCostsTheSame checks that a sign-in for a name that does not
// exist runs a full verification against the dummy hash.
func TestUnknownUserCostsTheSame(t *testing.T) {
	pbkdf2Iterations = 100000
	defer func() { pbkdf2Iterations = 1000 }()
	h := newHarness(t, noEnvAccounts)
	h.addUser("alice", store.RoleViewer)
	if !strings.HasPrefix(h.srv.dummyHash, "pbkdf2-sha256$100000$") {
		t.Fatalf("dummy hash %q", h.srv.dummyHash)
	}
	cost := func(name string) time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ {
			start := time.Now()
			if _, ok, _ := h.srv.checkLogin(name, "wrong-password"); ok {
				t.Fatal("accepted")
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	known, unknown := cost("alice"), cost("nobody")
	if unknown < known/2 || known < unknown/2 {
		t.Fatalf("known %v, unknown %v", known, unknown)
	}
}

func TestBootstrap(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "s.json"))
	if pw, err := Bootstrap(st, true); pw != "" || err != nil || len(st.Users()) != 0 {
		t.Fatalf("with env admin: %q %v", pw, err)
	}
	pw, err := Bootstrap(st, false)
	if err != nil || len(pw) != 24 || checkPassword(pw) != nil {
		t.Fatalf("bootstrap %q %v", pw, err)
	}
	users := st.Users()
	if len(users) != 1 || users[0].Username != "admin" || users[0].Role != store.RoleAdmin || users[0].Disabled || !verifyPassword(users[0].PasswordHash, pw) || strings.Contains(users[0].PasswordHash, pw) {
		t.Fatalf("users %+v", users)
	}
	if again, err := Bootstrap(st, false); again != "" || err != nil || len(st.Users()) != 1 {
		t.Fatalf("second start: %q %v", again, err)
	}

	// The bootstrapped account signs in and is not managed.
	srv, err := New(Config{Store: st, Dial: func(string, string) Switch { return newFake("", "") }, Static: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	acc, ok, _ := srv.checkLogin("admin", pw)
	if !ok || acc.Managed || acc.Role != store.RoleAdmin {
		t.Fatalf("login %+v %v", acc, ok)
	}
}

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

func TestChangeOwnPassword(t *testing.T) {
	h := newHarness(t)
	id := h.addUser("carol", store.RoleViewer)
	tok := h.login("carol", userPW)
	second := h.login("carol", userPW)

	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"wrong-password","new":"carols-new-password"}`), 400, `{"error":"current password is wrong"}`)
	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"`+userPW+`","new":"short"}`), 400, `{"error":"password must be 10 to 256 characters"}`)
	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"`+userPW+`"}`), 400, "")
	expect(t, h.do("POST", "/api/me/password", tok, `{"old":"x","new":"carols-new-password"}`), 400, "")
	expect(t, h.do("GET", "/api/me", second, ""), 200, "")

	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"`+userPW+`","new":"carols-new-password"}`), 204, "")
	expect(t, h.do("GET", "/api/me", tok, ""), 200, `{"username":"carol","role":"viewer","managed":false}`)
	expect(t, h.do("GET", "/api/me", second, ""), 401, "")
	expect(t, h.loginAs("carol", userPW), 401, "")
	h.login("carol", "carols-new-password")
	if u, _ := h.st.User(id); !verifyPassword(u.PasswordHash, "carols-new-password") {
		t.Fatal("hash not stored")
	}
}

func TestLegacyDataFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fsdeck.json")
	st, _ := store.Open(path)
	if _, err := st.Add(store.Instance{Name: "a", Host: "h", Port: 1, Password: eslPW}); err != nil {
		t.Fatal(err)
	}
	// The deployment of today: admin from the environment, no users in the
	// file. Nothing is bootstrapped and the file keeps its shape.
	if pw, _ := Bootstrap(st, true); pw != "" {
		t.Fatal("bootstrapped next to an environment admin")
	}
	h := &harness{t: t, st: st}
	srv, err := New(Config{AdminPassword: adminPW, Store: st, Dial: h.dial, Static: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, ok, _ := srv.checkLogin("admin", adminPW); !ok {
		t.Fatal("environment admin cannot sign in")
	}
	if _, ok, _ := srv.checkLogin("viewer", ""); ok {
		t.Fatal("viewer without a password")
	}
	reopened, _ := store.Open(path)
	if len(reopened.Users()) != 0 || len(reopened.List()) != 1 {
		t.Fatalf("file changed: %+v", reopened.Users())
	}
}

type routeCase struct {
	pattern      string
	method, path string
	body         string
	cap          capability
}

// routeTable lists every route. A route registered without an entry here
// fails TestRouteTable.
func routeTable(id string) []routeCase {
	p := "/api/instances/" + id
	unknownInstance := "/api/instances/0000000000000000"
	unknownUser := "/api/users/0000000000000000"
	return []routeCase{
		{"GET /healthz", "GET", "/healthz", "", capPublic},
		{"POST /api/login", "POST", "/api/login", `{"username":"nobody","password":"x"}`, capPublic},
		{"/", "GET", "/", "", capPublic},
		{"GET /api/me", "GET", "/api/me", "", capRead},
		{"POST /api/me/password", "POST", "/api/me/password", `{"current":"x","new":"short"}`, capRead},
		{"GET /api/version", "GET", "/api/version", "", capRead},
		{"GET /api/instances", "GET", "/api/instances", "", capRead},
		{"GET /api/instances/{id}/status", "GET", p + "/status", "", capRead},
		{"GET /api/instances/{id}/sofia", "GET", p + "/sofia", "", capRead},
		{"GET /api/instances/{id}/gateways", "GET", p + "/gateways", "", capRead},
		{"GET /api/instances/{id}/registrations", "GET", p + "/registrations", "", capRead},
		{"GET /api/instances/{id}/channels", "GET", p + "/channels", "", capRead},
		{"GET /api/instances/{id}/calls", "GET", p + "/calls", "", capRead},
		{"GET /api/instances/{id}/modules", "GET", p + "/modules", "", capRead},
		{"GET /api/instances/{id}/vars", "GET", p + "/vars", "", capRead},
		{"GET /api/instances/{id}/config", "GET", p + "/config", "", capRead},
		{"GET /api/instances/{id}/config/{name}", "GET", p + "/config/sofia.conf", "", capRead},
		{"GET /api/instances/{id}/dialplan", "GET", p + "/dialplan", "", capRead},
		{"GET /api/instances/{id}/dialplan/{name}", "GET", p + "/dialplan/default", "", capRead},
		{"GET /api/instances/{id}/directory", "GET", p + "/directory", "", capRead},
		{"GET /api/instances/{id}/directory/{name}", "GET", p + "/directory/10.0.0.1", "", capRead},
		{"GET /api/instances/{id}/history", "GET", p + "/history", "", capRead},
		{"GET /api/instances/{id}/stream", "GET", p + "/stream?loglevel=7", "", capRead},
		{"/api/", "GET", "/api/nope", "", capRead},
		{"DELETE /api/instances/{id}/channels/{uuid}", "DELETE", p + "/channels/abc-123", "", capOperate},
		{"POST /api/instances/{id}/actions", "POST", p + "/actions", `{"action":"reloadxml"}`, capOperate},
		{"POST /api/instances/{id}/console", "POST", p + "/console", `{"command":"status"}`, capConsole},
		{"POST /api/instances", "POST", "/api/instances", `{"name":"x","host":"h","port":1,"password":"p"}`, capInstances},
		{"PUT /api/instances/{id}", "PUT", unknownInstance, `{"name":"x","host":"h","port":1,"password":""}`, capInstances},
		{"DELETE /api/instances/{id}", "DELETE", unknownInstance, "", capInstances},
		{"GET /api/users", "GET", "/api/users", "", capUsers},
		{"POST /api/users", "POST", "/api/users", `{"username":"new.user","role":"viewer","password":"` + userPW + `"}`, capUsers},
		{"PUT /api/users/{id}", "PUT", unknownUser, `{"role":"viewer"}`, capUsers},
		{"DELETE /api/users/{id}", "DELETE", unknownUser, "", capUsers},
		{"GET /api/audit", "GET", "/api/audit", "", capUsers},
		{"POST /api/logout", "POST", "/api/logout", "", capRead}, // last: it ends the session
	}
}

func TestRouteTable(t *testing.T) {
	h := newHarness(t, noEnvAccounts)
	table := routeTable(h.id)
	var want, got []string
	for _, rc := range table {
		want = append(want, rc.pattern)
	}
	for p := range h.srv.routeCaps {
		got = append(got, p)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		t.Fatalf("registered routes differ from the test table:\n got %q\nwant %q", got, want)
	}
	for _, rc := range table {
		if h.srv.routeCaps[rc.pattern] != rc.cap {
			t.Errorf("%s: capability %d, want %d", rc.pattern, h.srv.routeCaps[rc.pattern], rc.cap)
		}
	}
}

// TestRoleMatrix walks every route with no session and with each role.
func TestRoleMatrix(t *testing.T) {
	// Capabilities per role, straight from the contract.
	contract := map[string][]capability{
		store.RoleViewer:   {capRead},
		store.RoleOperator: {capRead, capLogs, capOperate},
		store.RoleAdmin:    {capRead, capLogs, capOperate, capConsole, capInstances, capUsers},
	}
	for role, caps := range contract {
		for c := capRead; c <= capUsers; c++ {
			has := false
			for _, x := range caps {
				has = has || x == c
			}
			if allowed(role, c) != has {
				t.Errorf("%s capability %d: %v", role, c, !has)
			}
		}
	}
	if allowed("root", capRead) || allowed("", capRead) {
		t.Error("unknown role has a capability")
	}

	for _, role := range []string{"", store.RoleViewer, store.RoleOperator, store.RoleAdmin} {
		t.Run("role="+role, func(t *testing.T) {
			h := newHarness(t, noEnvAccounts)
			tok := ""
			if role != "" {
				h.addUser("u-"+role, role)
				tok = h.login("u-"+role, userPW)
			}
			for _, rc := range routeTable(h.id) {
				if rc.cap == capLogs {
					continue
				}
				var r response
				if strings.HasSuffix(rc.pattern, "/stream") {
					r = h.streamStatus(tok, rc.path)
				} else {
					r = h.do(rc.method, rc.path, tok, rc.body)
				}
				switch {
				case rc.cap == capPublic:
					// The sign-in answers for the credentials, not a session.
					if r.code != 200 && strings.TrimSpace(r.body) != `{"error":"invalid username or password"}` {
						t.Errorf("%s: %d %s", rc.pattern, r.code, r.body)
					}
				case role == "":
					if r.code != 401 {
						t.Errorf("%s without session: %d", rc.pattern, r.code)
					}
				case allowed(role, rc.cap):
					if r.code == 401 || r.code == 403 {
						t.Errorf("%s as %s: %d %s", rc.pattern, role, r.code, r.body)
					}
				default:
					if r.code != 403 || strings.TrimSpace(r.body) != `{"error":"permission denied"}` {
						t.Errorf("%s as %s: %d %s, want 403", rc.pattern, role, r.code, r.body)
					}
				}
			}
			// Refused requests changed nothing.
			sent, _, _, _ := h.fake(0).snapshot()
			for _, cmd := range sent {
				switch {
				case cmd == "uuid_kill abc-123" || cmd == "reloadxml":
					if !allowed(role, capOperate) {
						t.Errorf("%s caused %q", role, cmd)
					}
				case cmd == "status":
					if !allowed(role, capConsole) {
						t.Errorf("%s caused %q", role, cmd)
					}
				}
			}
			if _, ok := h.st.UserByName("new.user"); ok != allowed(role, capUsers) {
				t.Errorf("%s: user created %v", role, ok)
			}
			if n := len(h.st.List()); (n == 2) != allowed(role, capInstances) {
				t.Errorf("%s: %d instances", role, n)
			}
		})
	}
}

// streamStatus opens the event stream; openStream fails the test unless it
// answers 200 with an event stream.
func (h *harness) streamStatus(token, path string) response {
	h.t.Helper()
	if token == "" {
		return h.do("GET", path, "", "")
	}
	ch, cancel := h.openStream(token, strings.TrimPrefix(path, "/api/instances/"+h.id+"/stream"))
	defer cancel()
	next(h.t, ch)
	return response{code: 200}
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
