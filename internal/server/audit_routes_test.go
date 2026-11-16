package server

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/juki/freeswitch-console/internal/audit"
	"github.com/juki/freeswitch-console/internal/store"
)

func brief(e audit.Entry) string {
	return strings.Join([]string{e.User, e.Action, e.Target, e.Detail, e.Result, e.Error}, "|")
}

func TestAuditEvents(t *testing.T) {
	var path string
	h := newHarness(t, withAudit(t, &path))
	p := "/api/instances/" + h.id
	h.addUser("dana", store.RoleOperator)

	expect(t, h.loginAs("admin", "wrong-guess-1"), 401, "")
	expect(t, h.loginAs("Typed-A-Password-Here", "x"), 401, "")
	admin := h.login("admin", adminPW)
	r := h.do("POST", "/api/instances", admin, `{"name":"fs02","host":"10.0.0.9","port":8021,"password":"`+eslPW+`"}`)
	expect(t, r, 201, "")
	var created instanceView
	r.json(t, &created)
	expect(t, h.do("POST", "/api/instances", admin, `{"name":"","host":"h","port":1}`), 400, "")
	expect(t, h.do("PUT", "/api/instances/"+created.ID, admin, `{"name":"fs02b","host":"10.0.0.9","port":8022,"password":"new-esl-pw"}`), 200, "")
	expect(t, h.do("DELETE", "/api/instances/"+created.ID, admin, ""), 204, "")
	expect(t, h.do("DELETE", p+"/channels/abc-123", admin, ""), 200, "")
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"reloadxml"}`), 200, "")
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"sofia_rescan","profile":"internal"}`), 200, "")
	expect(t, h.do("POST", p+"/console", admin, `{"command":"  show <calls> count  "}`), 200, "")
	expect(t, h.do("POST", p+"/console", admin, `{"command":"bogus"}`), 200, "")
	r = h.do("POST", "/api/users", admin, `{"username":"erin","role":"viewer","password":"erins-password-1"}`)
	var erin userView
	r.json(t, &erin)
	expect(t, h.do("POST", "/api/users", admin, `{"username":"erin","role":"viewer","password":"erins-password-1"}`), 409, "")
	expect(t, h.do("PUT", "/api/users/"+erin.ID, admin, `{"role":"operator","password":"erins-password-2"}`), 200, "")
	expect(t, h.do("PUT", "/api/users/"+managedAdminID, admin, `{"disabled":true}`), 403, "")
	erinTok := h.login("erin", "erins-password-2")
	expect(t, h.do("POST", "/api/me/password", erinTok, `{"current":"nope-nope-nope","new":"erins-password-3"}`), 400, "")
	expect(t, h.do("POST", "/api/me/password", erinTok, `{"current":"erins-password-2","new":"erins-password-3"}`), 204, "")
	expect(t, h.do("POST", p+"/console", erinTok, `{"command":"status"}`), 403, "")
	expect(t, h.do("DELETE", "/api/users/"+erin.ID, admin, ""), 204, "")
	expect(t, h.do("POST", "/api/logout", admin, ""), 204, "")
	for i := 0; i < 5; i++ {
		h.loginAs("dana", "wrong-guess-2")
	}
	expect(t, h.loginAs("dana", userPW), 429, "")

	entries := readAudit(t, path)
	var got []string
	for _, e := range entries {
		if e.IP != "127.0.0.1" {
			t.Errorf("ip %q in %+v", e.IP, e)
		}
		got = append(got, brief(e))
	}
	want := []string{
		"admin|login.failed|admin|wrong password|denied|",
		"|login.failed||unknown user|denied|",
		"admin|login|admin||ok|",
		"admin|instance.create|fs02|id=" + created.ID + " host=10.0.0.9 port=8021 password=changed|ok|",
		"admin|instance.create||host=h port=1|error|invalid instance: name must be 1 to 64 characters",
		"admin|instance.update|fs02b|id=" + created.ID + " host=10.0.0.9 port=8022 password=changed|ok|",
		"admin|instance.delete|fs02b|id=" + created.ID + "|ok|",
		"admin|channel.hangup|fs01|uuid=abc-123|ok|",
		"admin|action.reloadxml|fs01||ok|",
		"admin|action.sofia_rescan|fs01|profile=internal|ok|",
		"admin|console.run|fs01|show <calls> count|ok|",
		"admin|console.run|fs01|bogus|error|-ERR bogus Command not found!",
		"admin|user.create|erin|id=" + erin.ID + " role=viewer|ok|",
		"admin|user.create|erin|role=viewer|error|username already exists",
		"admin|user.update|erin|id=" + erin.ID + " role=operator password=changed|ok|",
		"admin|user.update|admin|id=env-admin disabled=true|denied|" + errManaged,
		"erin|login|erin||ok|",
		"erin|password.change|erin||denied|current password is wrong",
		"erin|password.change|erin||ok|",
		"erin|console.run||POST " + p + "/console|denied|permission denied",
		"admin|user.delete|erin|id=" + erin.ID + "|ok|",
		"admin|logout|admin||ok|",
		"dana|login.failed|dana|wrong password|denied|",
		"dana|login.failed|dana|wrong password|denied|",
		"dana|login.failed|dana|wrong password|denied|",
		"dana|login.failed|dana|wrong password|denied|",
		"dana|login.failed|dana|wrong password|denied|",
		"dana|login.blocked|dana|retry in 60s|denied|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// No password, hash or session token is ever written.
	data, _ := os.ReadFile(path)
	for _, secret := range []string{adminPW, viewerPW, eslPW, "new-esl-pw", userPW, "erins-password", "wrong-guess", "nope-nope", "Typed-A-Password", "pbkdf2", admin, erinTok} {
		if strings.Contains(string(data), secret) {
			t.Errorf("audit log contains %q", secret)
		}
	}
}

func TestAuditRoute(t *testing.T) {
	var path string
	h := newHarness(t, withAudit(t, &path))
	admin := h.login("admin", adminPW)
	for i := 0; i < 1005; i++ {
		h.srv.cfg.Audit.Write(audit.Entry{User: "x", Action: "console.run", Detail: fmt.Sprintf("cmd %d", i), Result: audit.OK})
	}
	rowsOf := func(query string) []audit.Entry {
		t.Helper()
		r := h.do("GET", "/api/audit"+query, admin, "")
		expect(t, r, 200, "")
		var body struct {
			Enabled *bool
			Rows    []audit.Entry
		}
		r.json(t, &body)
		if body.Enabled == nil || !*body.Enabled || body.Rows == nil || !strings.HasPrefix(r.body, `{"enabled":true,"rows":[`) {
			t.Fatalf("body %.200s", r.body)
		}
		return body.Rows
	}
	if rows := rowsOf(""); len(rows) != 200 || rows[0].Detail != "cmd 1004" || rows[199].Detail != "cmd 805" {
		t.Fatalf("default: %d rows, first %+v", len(rows), rows[0])
	}
	if rows := rowsOf("?limit=5000"); len(rows) != 1000 {
		t.Fatalf("limit 5000: %d", len(rows))
	}
	if rows := rowsOf("?limit=2"); len(rows) != 2 || rows[1].Detail != "cmd 1003" {
		t.Fatalf("limit 2: %+v", rows)
	}
	// "cmd 10", "cmd 100" to "cmd 109", "cmd 1000" to "cmd 1004".
	if rows := rowsOf("?q=CMD%2010&limit=1000"); len(rows) != 16 || rows[0].Detail != "cmd 1004" || rows[15].Detail != "cmd 10" {
		var d []string
		for _, r := range rows {
			d = append(d, r.Detail)
		}
		t.Fatalf("q: %v", d)
	}
	if rows := rowsOf("?q=login"); len(rows) != 1 || rows[0].Action != "login" {
		t.Fatalf("q login: %+v", rows)
	}
	if rows := rowsOf("?q=nothing-matches"); len(rows) != 0 {
		t.Fatalf("q none: %+v", rows)
	}
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=x"} {
		expect(t, h.do("GET", "/api/audit"+q, admin, ""), 400, `{"error":"limit must be a positive number"}`)
	}
}

func TestAuditDisabled(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	expect(t, h.do("POST", "/api/instances/"+h.id+"/console", admin, `{"command":"status"}`), 200, "")
	expect(t, h.do("GET", "/api/audit", admin, ""), 200, `{"enabled":false,"rows":[]}`)
}
