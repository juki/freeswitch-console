package server

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/juki/freeswitch-console/internal/store"
)

// TestActionCaps pins the role split of the contract.
func TestActionCaps(t *testing.T) {
	operator := map[string]bool{
		"reloadxml/": true, "reloadacl/": true, "sofia_rescan/": true,
		"sofia_profile/rescan": true, "sofia_gateway/register": true, "sofia_gateway/unregister": true,
	}
	n := 0
	for action, ops := range actionCaps {
		for op, c := range ops {
			n++
			want := capControl
			if operator[action+"/"+op] {
				want = capOperate
			}
			if c != want {
				t.Errorf("%s %s: capability %d, want %d", action, op, c, want)
			}
		}
	}
	if n != 21 {
		t.Fatalf("%d operations", n)
	}
	if !allowed(store.RoleAdmin, capControl) || allowed(store.RoleOperator, capControl) || allowed(store.RoleViewer, capOperate) {
		t.Fatal("role table")
	}
}

func TestActions(t *testing.T) {
	var auditPath string
	h := newHarness(t, withAudit(t, &auditPath))
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id + "/actions"
	f := h.fake(0)
	f.set(func(f *fakeSwitch) {
		f.sent = nil
		f.replies["module_exists x"] = ""
		f.replies["load mod_nope"] = "-ERR [module load file routine returned an error]\n"
	})
	ok := map[string]string{
		`{"action":"reloadxml"}`:                                                            "reloadxml",
		`{"action":"reloadacl"}`:                                                            "reloadacl",
		`{"action":"sofia_rescan","profile":"internal"}`:                                    "sofia profile internal rescan",
		`{"action":"sofia_profile","profile":"internal","op":"rescan"}`:                     "sofia profile internal rescan",
		`{"action":"sofia_profile","profile":"external","op":"start"}`:                      "sofia profile external start",
		`{"action":"sofia_profile","profile":"external","op":"stop"}`:                       "sofia profile external stop",
		`{"action":"sofia_profile","profile":"external","op":"restart"}`:                    "sofia profile external restart",
		`{"action":"sofia_profile","profile":"internal","op":"flush_inbound_reg"}`:          "sofia profile internal flush_inbound_reg",
		`{"action":"sofia_gateway","profile":"external","gateway":"gw.1","op":"kill"}`:      "sofia profile external killgw gw.1",
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"register"}`:   "sofia profile external register gw1",
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"unregister"}`: "sofia profile external unregister gw1",
		`{"action":"sofia_siptrace","profile":"internal","on":true}`:                        "sofia profile internal siptrace on",
		`{"action":"sofia_siptrace","profile":"external","on":false}`:                       "sofia profile external siptrace off",
		`{"action":"module","module":"mod_sofia","op":"reload"}`:                            "reload mod_sofia",
		`{"action":"module","module":"mod_xml_curl","op":"load"}`:                           "load mod_xml_curl",
		`{"action":"module","module":"mod_xml_curl","op":"unload"}`:                         "unload mod_xml_curl",
		`{"action":"fsctl","op":"loglevel","value":7}`:                                      "fsctl loglevel 7",
		`{"action":"fsctl","op":"loglevel","value":"0"}`:                                    "fsctl loglevel 0",
		`{"action":"fsctl","op":"max_sessions","value":100000}`:                             "fsctl max_sessions 100000",
		`{"action":"fsctl","op":"sps","value":"1"}`:                                         "fsctl sps 1",
		`{"action":"fsctl","op":"pause"}`:                                                   "fsctl pause",
		`{"action":"fsctl","op":"resume"}`:                                                  "fsctl resume",
		`{"action":"setvar","name":"outbound_caller_id","value":"Acme <1000>"}`:             "global_setvar outbound_caller_id=Acme <1000>",
		`{"action":"setvar","name":"default_password","value":"s3cret-v"}`:                  "global_setvar default_password=s3cret-v",
		`{"action":"setvar","name":"empty.var","value":""}`:                                 "global_setvar empty.var=",
	}
	for body, cmd := range ok {
		f.set(func(f *fakeSwitch) { f.sent = nil })
		expect(t, h.do("POST", p, admin, body), 200, "")
		if sent, _, _, _ := f.snapshot(); !reflect.DeepEqual(sent, []string{"bgapi " + cmd}) {
			t.Errorf("%s sent %q, want %q with Background", body, sent, cmd)
		}
	}
	f.set(func(f *fakeSwitch) { f.sent = nil })
	expect(t, h.do("POST", p, admin, `{"action":"module","module":"mod_nope","op":"load"}`), 502, `{"error":"-ERR [module load file routine returned an error]"}`)

	f.set(func(f *fakeSwitch) { f.sent = nil })
	for _, body := range []string{
		`{"action":"shutdown"}`,
		`{"action":"reloadxml","op":"now"}`,
		`{"action":"sofia_profile","profile":"internal"}`,
		`{"action":"sofia_profile","profile":"internal","op":"siptrace"}`,
		`{"action":"sofia_profile","profile":"in ternal","op":"stop"}`,
		`{"action":"sofia_profile","profile":"internal;reloadxml","op":"stop"}`,
		`{"action":"sofia_profile","profile":"","op":"rescan"}`,
		`{"action":"sofia_gateway","profile":"external","op":"kill"}`,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw 1","op":"kill"}`,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"delete"}`,
		`{"action":"sofia_siptrace","profile":"internal"}`,
		`{"action":"sofia_siptrace","profile":"internal","on":"on"}`,
		`{"action":"sofia_siptrace","profile":"internal","on":null}`,
		`{"action":"sofia_siptrace","profile":"in ternal","on":true}`,
		`{"action":"sofia_siptrace","profile":"","on":true}`,
		`{"action":"sofia_siptrace","profile":"internal","op":"on","on":true}`,
		`{"action":"module","module":"sofia","op":"load"}`,
		`{"action":"module","module":"mod_Sofia","op":"load"}`,
		`{"action":"module","module":"mod_x y","op":"load"}`,
		`{"action":"module","module":"mod_x","op":"remove"}`,
		`{"action":"fsctl","op":"loglevel","value":8}`,
		`{"action":"fsctl","op":"loglevel","value":-1}`,
		`{"action":"fsctl","op":"loglevel"}`,
		`{"action":"fsctl","op":"loglevel","value":"7 extra"}`,
		`{"action":"fsctl","op":"loglevel","value":1.5}`,
		`{"action":"fsctl","op":"max_sessions","value":0}`,
		`{"action":"fsctl","op":"max_sessions","value":100001}`,
		`{"action":"fsctl","op":"sps","value":10001}`,
		`{"action":"fsctl","op":"pause","value":1}`,
		`{"action":"fsctl","op":"shutdown"}`,
		`{"action":"fsctl","op":"hupall"}`,
		`{"action":"setvar","name":"a b","value":"x"}`,
		`{"action":"setvar","name":"","value":"x"}`,
		`{"action":"setvar","name":"a=b","value":"x"}`,
		`{"action":"setvar","name":"a","value":"x\ny"}`,
		`{"action":"setvar","name":"a","value":"x\rapi shutdown"}`,
		`{"action":"setvar","name":"a","value":"` + strings.Repeat("v", 1025) + `"}`,
		`{"action":"setvar","name":"a","value":true}`,
		`{"action":"setvar","name":"a","value":"x","extra":1}`,
	} {
		expect(t, h.do("POST", p, admin, body), 400, "")
	}
	if sent, _, _, _ := f.snapshot(); len(sent) != 0 {
		t.Fatalf("invalid requests sent %q", sent)
	}
	expect(t, h.do("POST", p, admin, `{"action":"setvar","name":"a","value":"`+strings.Repeat("é", 1024)+`"}`), 200, "")

	// A viewer gets 403 for every operation and sends nothing.
	viewer := h.login("viewer", viewerPW)
	f.set(func(f *fakeSwitch) { f.sent = nil })
	for body := range ok {
		expect(t, h.do("POST", p, viewer, body), 403, `{"error":"permission denied"}`)
	}
	if sent, _, _, _ := f.snapshot(); len(sent) != 0 {
		t.Fatalf("viewer sent %q", sent)
	}

	data, _ := os.ReadFile(auditPath)
	for _, want := range []string{
		`"action":"action.sofia_gateway","target":"fs01","detail":"op=kill profile=external gateway=gw.1","result":"ok"`,
		`"action":"action.fsctl","target":"fs01","detail":"op=loglevel value=7","result":"ok"`,
		`"action":"action.sofia_siptrace","target":"fs01","detail":"profile=internal siptrace=on","result":"ok"`,
		`"action":"action.sofia_siptrace","target":"fs01","detail":"profile=external siptrace=off","result":"ok"`,
		`"action":"action.sofia_siptrace","target":"fs01","detail":"","result":"error","error":"on must be true or false"`,
		`"action":"action.module","target":"fs01","detail":"op=load module=mod_nope","result":"error","error":"-ERR [module load file routine returned an error]"`,
		`"action":"action.setvar","target":"fs01","detail":"name=outbound_caller_id value=Acme <1000>","result":"ok"`,
		`"action":"action.setvar","target":"fs01","detail":"name=default_password value=********","result":"ok"`,
		`"action":"action.fsctl","target":"fs01","detail":"op=loglevel","result":"error","error":"fsctl loglevel needs a value from 0 to 7"`,
		`"user":"viewer","ip":"127.0.0.1","action":"action.*","target":"","detail":"POST ` + p + `","result":"denied"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("audit lacks %s", want)
		}
	}
	if strings.Contains(string(data), "s3cret-v") {
		t.Error("audit holds a secret variable value")
	}
}

// TestOperatorActions runs every operation as an operator: the table decides.
func TestOperatorActions(t *testing.T) {
	h := newHarness(t, noEnvAccounts)
	h.addUser("op", store.RoleOperator)
	tok := h.login("op", userPW)
	p := "/api/instances/" + h.id + "/actions"
	f := h.fake(0)
	for body, code := range map[string]int{
		`{"action":"reloadxml"}`:                                                            200,
		`{"action":"reloadacl"}`:                                                            200,
		`{"action":"sofia_rescan","profile":"internal"}`:                                    200,
		`{"action":"sofia_profile","profile":"internal","op":"rescan"}`:                     200,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"register"}`:   200,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"unregister"}`: 200,
		`{"action":"sofia_profile","profile":"internal","op":"restart"}`:                    403,
		`{"action":"sofia_profile","profile":"internal","op":"stop"}`:                       403,
		`{"action":"sofia_profile","profile":"internal","op":"start"}`:                      403,
		`{"action":"sofia_profile","profile":"internal","op":"flush_inbound_reg"}`:          403,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"kill"}`:       403,
		`{"action":"module","module":"mod_sofia","op":"reload"}`:                            403,
		`{"action":"fsctl","op":"pause"}`:                                                   403,
		`{"action":"setvar","name":"a","value":"b"}`:                                        403,
		`{"action":"sofia_siptrace","profile":"internal","on":true}`:                        403,
		`{"action":"sofia_siptrace","profile":"bad name"}`:                                  403,
		`{"action":"sofia_gateway","profile":"bad name","gateway":"gw1","op":"kill"}`:       403, // refused before validation
	} {
		f.set(func(f *fakeSwitch) { f.sent = nil })
		r := h.do("POST", p, tok, body)
		expect(t, r, code, "")
		sent, _, _, _ := f.snapshot()
		if (len(sent) == 1) != (code == 200) {
			t.Errorf("%s sent %q", body, sent)
		}
	}
}
