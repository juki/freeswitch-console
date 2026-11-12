package server

import (
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"fsdeck/internal/store"
)

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

func TestChangeOwnPassword(t *testing.T) {
	h := newHarness(t, noEnvAccounts)
	id := h.addUser("viewer", store.RoleViewer)
	tok := h.login("viewer", userPW)
	second := h.login("viewer", userPW)

	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"wrong-password","new":"carols-new-password"}`), 400, `{"error":"current password is wrong"}`)
	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"`+userPW+`","new":"short"}`), 400, `{"error":"password must be 10 to 256 characters"}`)
	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"`+userPW+`"}`), 400, "")
	expect(t, h.do("POST", "/api/me/password", tok, `{"old":"x","new":"carols-new-password"}`), 400, "")
	expect(t, h.do("GET", "/api/me", second, ""), 200, "")

	expect(t, h.do("POST", "/api/me/password", tok, `{"current":"`+userPW+`","new":"carols-new-password"}`), 204, "")
	expect(t, h.do("GET", "/api/me", tok, ""), 200, `{"username":"viewer","role":"viewer","managed":false}`)
	expect(t, h.do("GET", "/api/me", second, ""), 401, "")
	expect(t, h.loginAs("viewer", userPW), 401, "")
	h.login("viewer", "carols-new-password")
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
	feature      string // paid feature, "" for none
}

// routeTable lists every route of this build: the core routes here, the
// paid ones from paidRoutes (empty in a community build). A route
// registered without an entry fails TestRouteTable.
func routeTable(id string) []routeCase {
	p := "/api/instances/" + id
	unknownInstance := "/api/instances/0000000000000000"
	core := []routeCase{
		{"GET /healthz", "GET", "/healthz", "", capPublic, ""},
		{"POST /api/login", "POST", "/api/login", `{"username":"nobody","password":"x"}`, capPublic, ""},
		{"/", "GET", "/", "", capPublic, ""},
		{"GET /api/me", "GET", "/api/me", "", capRead, ""},
		{"POST /api/me/password", "POST", "/api/me/password", `{"current":"x","new":"short"}`, capRead, ""},
		{"GET /api/version", "GET", "/api/version", "", capRead, ""},
		{"GET /api/license", "GET", "/api/license", "", capRead, ""},
		{"GET /api/instances", "GET", "/api/instances", "", capRead, ""},
		{"GET /api/instances/{id}/status", "GET", p + "/status", "", capRead, ""},
		{"GET /api/instances/{id}/sofia", "GET", p + "/sofia", "", capRead, ""},
		{"GET /api/instances/{id}/gateways", "GET", p + "/gateways", "", capRead, ""},
		{"GET /api/instances/{id}/registrations", "GET", p + "/registrations", "", capRead, ""},
		{"GET /api/instances/{id}/channels", "GET", p + "/channels", "", capRead, ""},
		{"GET /api/instances/{id}/calls", "GET", p + "/calls", "", capRead, ""},
		{"GET /api/instances/{id}/modules", "GET", p + "/modules", "", capRead, ""},
		{"GET /api/instances/{id}/vars", "GET", p + "/vars", "", capRead, ""},
		{"GET /api/instances/{id}/config", "GET", p + "/config", "", capRead, ""},
		{"GET /api/instances/{id}/config/{name}", "GET", p + "/config/sofia.conf", "", capRead, ""},
		{"GET /api/instances/{id}/dialplan", "GET", p + "/dialplan", "", capRead, ""},
		{"GET /api/instances/{id}/dialplan/{name}", "GET", p + "/dialplan/default", "", capRead, ""},
		{"GET /api/instances/{id}/directory", "GET", p + "/directory", "", capRead, ""},
		{"GET /api/instances/{id}/directory/{name}", "GET", p + "/directory/10.0.0.1", "", capRead, ""},
		{"GET /api/instances/{id}/history", "GET", p + "/history", "", capRead, ""},
		{"GET /api/instances/{id}/stream", "GET", p + "/stream?loglevel=7", "", capRead, ""},
		{"/api/", "GET", "/api/nope", "", capRead, ""},
		{"DELETE /api/instances/{id}/channels/{uuid}", "DELETE", p + "/channels/abc-123", "", capOperate, ""},
		{"POST /api/instances/{id}/actions", "POST", p + "/actions", `{"action":"reloadxml"}`, capOperate, ""},
		{"POST /api/instances/{id}/console", "POST", p + "/console", `{"command":"status"}`, capConsole, ""},
		{"POST /api/instances/{id}/console/complete", "POST", p + "/console/complete", `{"line":"show ch"}`, capConsole, ""},
		{"GET /api/instances/{id}/console/commands", "GET", p + "/console/commands", "", capConsole, ""},
		{"POST /api/instances", "POST", "/api/instances", `{"name":"x","host":"h","port":1,"password":"p"}`, capInstances, ""},
		{"PUT /api/instances/{id}", "PUT", unknownInstance, `{"name":"x","host":"h","port":1,"password":""}`, capInstances, ""},
		{"DELETE /api/instances/{id}", "DELETE", unknownInstance, "", capInstances, ""},
	}
	// Last: it ends the session.
	logout := routeCase{"POST /api/logout", "POST", "/api/logout", "", capRead, ""}
	return append(append(core, paidRoutes(id)...), logout)
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
		if h.srv.routeCaps[rc.pattern] != rc.cap || h.srv.routeFeatures[rc.pattern] != rc.feature {
			t.Errorf("%s: capability %d feature %q, want %d %q", rc.pattern, h.srv.routeCaps[rc.pattern], h.srv.routeFeatures[rc.pattern], rc.cap, rc.feature)
		}
	}
}

// matrixRun is one licence situation of the role matrix.
type matrixRun struct {
	name     string
	opts     []func(*Config)
	features bool // the licence grants every feature
}

// TestRoleMatrix walks every route with no session and with each role, in
// every licence situation of this build (matrixRuns).
func TestRoleMatrix(t *testing.T) {
	// Capabilities per role, straight from the contract.
	contract := map[string][]capability{
		store.RoleViewer:   {capRead},
		store.RoleOperator: {capRead, capLogs, capOperate},
		store.RoleAdmin:    {capRead, capLogs, capOperate, capControl, capConsole, capInstances, capUsers, capFiles},
	}
	if !paidBuild {
		delete(contract, store.RoleOperator)
	}
	for role, caps := range contract {
		for c := capRead; c <= capFiles; c++ {
			has := false
			for _, x := range caps {
				has = has || x == c
			}
			if allowed(role, c) != has {
				t.Errorf("%s capability %d: %v", role, c, !has)
			}
		}
	}
	if allowed("root", capRead) || allowed("", capRead) || !paidBuild && allowed(store.RoleOperator, capRead) {
		t.Error("unknown role has a capability")
	}

	for _, run := range matrixRuns(t) {
		for _, role := range []string{"", store.RoleViewer, store.RoleOperator, store.RoleAdmin} {
			t.Run(run.name+"/role="+role, func(t *testing.T) {
				h := newHarness(t, append([]func(*Config){noEnvAccounts}, run.opts...)...)
				tok := ""
				if role != "" {
					// File accounts named after their role: admin and viewer
					// sign in without a licence too.
					h.addUser(role, role)
					if role == store.RoleOperator && !run.features {
						expect(t, h.loginAs(role, userPW), 403, `{"error":"`+errUnlicensedAccount+`"}`)
						return
					}
					tok = h.login(role, userPW)
				}
				for _, rc := range routeTable(h.id) {
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
					case !allowed(role, rc.cap):
						if r.code != 403 || strings.TrimSpace(r.body) != `{"error":"permission denied"}` {
							t.Errorf("%s as %s: %d %s, want 403", rc.pattern, role, r.code, r.body)
						}
					case rc.feature != "" && !run.features:
						if r.code != 402 || strings.TrimSpace(r.body) != `{"error":"this feature needs a licence","feature":"`+rc.feature+`"}` {
							t.Errorf("%s as %s: %d %s, want 402", rc.pattern, role, r.code, r.body)
						}
					default:
						if r.code == 401 || r.code == 402 || r.code == 403 {
							t.Errorf("%s as %s: %d %s", rc.pattern, role, r.code, r.body)
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
				if _, ok := h.st.UserByName("new.user"); ok != (allowed(role, capUsers) && run.features) {
					t.Errorf("%s: user created %v", role, ok)
				}
				if n := len(h.st.List()); (n == 2) != allowed(role, capInstances) {
					t.Errorf("%s: %d instances", role, n)
				}
			})
		}
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
