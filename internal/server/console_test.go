package server

import (
	"context"
	"strings"
	"testing"

	"github.com/juki/freeswitch-console/internal/esl"
)

func TestConsoleCompletion(t *testing.T) {
	var auditPath string
	h := newHarness(t, withAudit(t, &auditPath))
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id + "/console"
	f := h.fake(0)
	f.set(func(f *fakeSwitch) {
		f.replies["console_complete_xml c=14;sofia profile x"] = "<?xml version=\"1.0\"?>\n<complete>\n  <match>external</match>\n  <match>internal</match>\n  <match>external</match>\n  <write length=\"0\"></write>\n</complete>\n"
		f.replies["console_complete_xml c=7;show chx"] = "<?xml version=\"1.0\"?>\n<complete>\n  <match>channels</match>\n  <match>chat</match>\n  <write length=\"2\">cha</write>\n</complete>\n"
		f.replies["console_complete_xml c=3;zzzx"] = "<?xml version=\"1.0\"?>\n<complete>\n  <write length=\"3\"></write>\n</complete>\n"
		f.replies["console_complete_xml c=0;"] = "<complete><match>acl</match><write length=\"0\"></write></complete>"
		f.replies["console_complete_xml c=3;badx"] = "<complete><match>x</match>"
		f.replies["show api as json"] = `{"row_count":2,"rows":[{"name":"status","description":"Show current status","syntax":"","ikey":"mod_commands"},{"name":"acl","description":"Compare an ip to an acl list","syntax":"<ip> <list_name>","ikey":"mod_commands"}]}`
		f.sent = nil
	})
	expect(t, h.do("POST", p+"/complete", admin, `{"line":"sofia profile "}`), 200, `{"matches":["external","internal"],"replace":0,"text":""}`)
	expect(t, h.do("POST", p+"/complete", admin, `{"line":"show ch"}`), 200, `{"matches":["channels","chat"],"replace":2,"text":"cha"}`)
	expect(t, h.do("POST", p+"/complete", admin, `{"line":"zzz"}`), 200, `{"matches":[],"replace":0,"text":""}`)
	expect(t, h.do("POST", p+"/complete", admin, `{"line":""}`), 200, `{"matches":["acl"],"replace":0,"text":""}`)
	expect(t, h.do("POST", p+"/complete", admin, `{}`), 200, `{"matches":["acl"],"replace":0,"text":""}`)
	expect(t, h.do("POST", p+"/complete", admin, `{"line":"bad"}`), 502, "")
	expect(t, h.do("GET", p+"/commands", admin, ""), 200, `{"rows":[{"name":"acl","description":"Compare an ip to an acl list","syntax":"\u003cip\u003e \u003clist_name\u003e"},{"name":"status","description":"Show current status","syntax":""}]}`)

	f.set(func(f *fakeSwitch) { f.sent = nil })
	for _, body := range []string{`{"line":"a\nb"}`, `{"line":"a\rapi shutdown"}`, `{"line":"` + strings.Repeat("é", 513) + `"}`, `{"line":1}`, `{"line":"a","x":1}`} {
		expect(t, h.do("POST", p+"/complete", admin, body), 400, "")
	}
	if sent, _, _, _ := f.snapshot(); len(sent) != 0 {
		t.Fatalf("invalid lines sent %q", sent)
	}

	// Not audited when allowed; refused for a viewer, and that is audited.
	viewer := h.login("viewer", viewerPW)
	expect(t, h.do("POST", p+"/complete", viewer, `{"line":"show ch"}`), 403, `{"error":"permission denied"}`)
	expect(t, h.do("GET", p+"/commands", viewer, ""), 403, "")
	data := string(must(t, auditPath))
	if strings.Contains(data, `"user":"admin","ip":"127.0.0.1","action":"console.complete"`) || strings.Contains(data, `"user":"admin","ip":"127.0.0.1","action":"console.commands"`) {
		t.Fatal("allowed completion audited")
	}
	for _, want := range []string{`"user":"viewer","ip":"127.0.0.1","action":"console.complete","target":"","detail":"POST ` + p + `/complete","result":"denied"`, `"action":"console.commands"`} {
		if !strings.Contains(data, want) {
			t.Errorf("audit lacks %s", want)
		}
	}

	f.set(func(f *fakeSwitch) { f.state = esl.State{}; f.sent = nil })
	expect(t, h.do("POST", p+"/complete", admin, `{"line":"show ch"}`), 503, `{"error":"instance not connected"}`)
	expect(t, h.do("GET", p+"/commands", admin, ""), 503, `{"error":"instance not connected"}`)
	expect(t, h.do("POST", "/api/instances/0000000000000000/console/complete", admin, `{"line":""}`), 404, `{"error":"instance not found"}`)
	if sent, _, _, _ := f.snapshot(); len(sent) != 0 {
		t.Fatalf("sent while disconnected %q", sent)
	}
}

// TestBackgroundDeadline: a console command or an action that passes its
// deadline answers 504 and is audited as failed; the instance stays up.
func TestBackgroundDeadline(t *testing.T) {
	var auditPath string
	h := newHarness(t, withAudit(t, &auditPath))
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	f := h.fake(0)
	f.set(func(f *fakeSwitch) { f.apiErr = context.DeadlineExceeded })
	const console = "the command is still running on FreeSWITCH after 60s; its result will not be shown"
	const action = "the command is still running on FreeSWITCH after 15s; its result will not be shown"
	expect(t, h.do("POST", p+"/console", admin, `{"command":"originate x &park()"}`), 504, `{"error":"`+console+`"}`)
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"reloadxml"}`), 504, `{"error":"`+action+`"}`)
	if !f.State().Connected {
		t.Fatal("disconnected")
	}
	data := string(must(t, auditPath))
	for _, want := range []string{
		`"action":"console.run","target":"fs01","detail":"originate x &park()","result":"error","error":"` + console + `"`,
		`"action":"action.reloadxml","target":"fs01","detail":"","result":"error","error":"` + action + `"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("audit lacks %s:\n%s", want, data)
		}
	}
}
