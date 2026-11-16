package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juki/freeswitch-console/internal/conffs"
)

const agentToken = "agent-token-0123456789"

// confDir makes a FreeSWITCH-like conf directory.
func confDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "conf")
	os.MkdirAll(filepath.Join(root, "sip_profiles"), 0o755)
	os.WriteFile(filepath.Join(root, "vars.xml"), []byte(`<include><X-PRE-PROCESS cmd="set" data="default_password=1234"/></include>`), 0o644)
	os.WriteFile(filepath.Join(root, "sip_profiles", "internal.xml"), []byte(`<profile name="internal"/>`), 0o644)
	return root
}

func withBackups(t *testing.T) func(*Config) {
	dir := filepath.Join(t.TempDir(), "backups")
	return func(c *Config) { c.BackupDir = dir }
}

func TestInstanceConfigAccess(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id

	r := h.do("GET", p+"/status", admin, "")
	expect(t, r, 200, "")
	r = h.do("GET", "/api/instances", admin, "")
	if !strings.Contains(r.body, `"configMode":"","configPath":"","agentURL":""`) {
		t.Fatalf("view %s", r.body)
	}

	r = h.do("PUT", p, admin, `{"name":"fs01","host":"10.0.0.5","port":8021,"password":"","configMode":"agent","agentURL":"https://fs01.example.com:8025","agentToken":"`+agentToken+`"}`)
	expect(t, r, 200, "")
	if strings.Contains(r.body, agentToken) || !strings.Contains(r.body, `"configMode":"agent","configPath":"","agentURL":"https://fs01.example.com:8025"`) || strings.Contains(r.body, "agentToken") {
		t.Fatalf("update %s", r.body)
	}
	// Config access alone does not reconnect.
	h.mu.Lock()
	dialed := len(h.dialed)
	h.mu.Unlock()
	if dialed != 1 {
		t.Fatalf("redialed %d times", dialed)
	}
	in, _ := h.st.Get(h.id)
	if in.AgentToken != agentToken {
		t.Fatal("token not stored")
	}
	// Without configMode the access stays; an empty token keeps the token.
	expect(t, h.do("PUT", p, admin, `{"name":"fs01b","host":"10.0.0.5","port":8021,"password":""}`), 200, "")
	if in, _ := h.st.Get(h.id); in.ConfigMode != "agent" || in.AgentToken != agentToken {
		t.Fatalf("kept %+v", in)
	}
	expect(t, h.do("PUT", p, admin, `{"name":"fs01b","host":"10.0.0.5","port":8021,"password":"","configMode":"agent","agentURL":"http://10.0.0.5:8025","agentToken":""}`), 200, "")
	if in, _ := h.st.Get(h.id); in.AgentURL != "http://10.0.0.5:8025" || in.AgentToken != agentToken {
		t.Fatalf("token kept %+v", in)
	}
	for _, body := range []string{
		`{"name":"a","host":"h","port":1,"configMode":"ftp"}`,
		`{"name":"a","host":"h","port":1,"configMode":"local","configPath":"relative/conf"}`,
		`{"name":"a","host":"h","port":1,"configMode":"local","configPath":"/etc/../etc/freeswitch"}`,
		`{"name":"a","host":"h","port":1,"configMode":"agent","agentURL":"ftp://x","agentToken":"` + agentToken + `"}`,
		`{"name":"a","host":"h","port":1,"configMode":"agent","agentURL":"https://user:pw@x","agentToken":"` + agentToken + `"}`,
		`{"name":"a","host":"h","port":1,"configMode":"agent","agentURL":"https://x/?t=1","agentToken":"` + agentToken + `"}`,
		`{"name":"a","host":"h","port":1,"configMode":"agent","agentURL":"https://x","agentToken":"short"}`,
		`{"name":"a","host":"h","port":1,"configMode":"agent","agentURL":"https://x"}`,
	} {
		expect(t, h.do("POST", "/api/instances", admin, body), 400, "")
	}
	r = h.do("POST", "/api/instances", admin, `{"name":"b","host":"h","port":1,"configMode":"local","configPath":"/etc/freeswitch","agentURL":"https://ignored","agentToken":"ignored-token-1234567"}`)
	expect(t, r, 201, "")
	if !strings.Contains(r.body, `"configMode":"local","configPath":"/etc/freeswitch","agentURL":""`) {
		t.Fatalf("local %s", r.body)
	}
	// Switching off clears everything.
	expect(t, h.do("PUT", p, admin, `{"name":"fs01b","host":"10.0.0.5","port":8021,"password":"","configMode":""}`), 200, "")
	if in, _ := h.st.Get(h.id); in.ConfigMode != "" || in.AgentURL != "" || in.AgentToken != "" {
		t.Fatalf("cleared %+v", in)
	}
}

func TestFilesLocal(t *testing.T) {
	var auditPath string
	h := newHarness(t, withBackups(t), withAudit(t, &auditPath))
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	expect(t, h.do("GET", p+"/files?path=", admin, ""), 409, `{"error":"config access is not set up for this instance"}`)
	expect(t, h.do("GET", "/api/instances/0000000000000000/files?path=", admin, ""), 404, `{"error":"instance not found"}`)
	root := confDir(t)
	expect(t, h.do("PUT", p, admin, `{"name":"fs01","host":"10.0.0.5","port":8021,"password":"","configMode":"local","configPath":"`+root+`"}`), 200, "")

	expect(t, h.do("GET", p+"/files?path=", admin, ""), 200, "")
	r := h.do("GET", p+"/files?path=", admin, "")
	var list struct{ Rows []conffs.Entry }
	r.json(t, &list)
	if len(list.Rows) != 2 || list.Rows[0].Name != "sip_profiles" || !list.Rows[0].Dir || list.Rows[1].Name != "vars.xml" {
		t.Fatalf("list %s", r.body)
	}
	if !strings.Contains(r.body, `{"name":"vars.xml","dir":false,"size":`) {
		t.Fatalf("list shape %s", r.body)
	}

	r = h.do("GET", p+"/files/content?path=vars.xml", admin, "")
	expect(t, r, 200, "")
	var f conffs.File
	r.json(t, &f)
	var raw map[string]any
	json.Unmarshal([]byte(r.body), &raw)
	if len(raw) != 5 || f.Path != "vars.xml" || !strings.Contains(f.Content, "default_password=1234") || len(f.SHA256) != 64 {
		t.Fatalf("read %s", r.body)
	}

	put := func(content, base string) response {
		b, _ := json.Marshal(map[string]string{"path": "vars.xml", "content": content, "baseSha256": base})
		return h.do("PUT", p+"/files/content", admin, string(b))
	}
	r = put("<include>\n<broken>\n</include>", f.SHA256)
	expect(t, r, 422, "")
	var bad struct {
		Error string
		Line  *int
	}
	r.json(t, &bad)
	if bad.Error != "the XML is not well-formed: element <broken> closed by </include>" || bad.Line == nil || *bad.Line != 3 {
		t.Fatalf("422 %s", r.body)
	}
	r = put("<include/>", strings.Repeat("0", 64))
	expect(t, r, 409, `{"error":"the file has changed since it was read","currentSha256":"`+f.SHA256+`"}`)
	r = put("<include/>", f.SHA256)
	expect(t, r, 200, "")
	var w1 conffs.Written
	r.json(t, &w1)
	raw = nil
	json.Unmarshal([]byte(r.body), &raw)
	if len(raw) != 4 || w1.Path != "vars.xml" || w1.Size != 10 {
		t.Fatalf("write %s", r.body)
	}
	// A file of the maximum size passes the body limit.
	big := "<include><!--" + strings.Repeat("\"", conffs.MaxSize-26) + "--></include>"
	if len(big) != conffs.MaxSize {
		t.Fatal(len(big))
	}
	r = put(big, w1.SHA256)
	expect(t, r, 200, "")
	var w2 conffs.Written
	r.json(t, &w2)
	expect(t, put(big+"x", w2.SHA256), 400, "")

	r = h.do("GET", p+"/files/history?path=vars.xml", admin, "")
	var hist struct{ Rows []conffs.Backup }
	r.json(t, &hist)
	if len(hist.Rows) != 2 || hist.Rows[0].SHA256 != w1.SHA256 || hist.Rows[1].SHA256 != f.SHA256 || hist.Rows[0].User != "admin" {
		t.Fatalf("history %s", r.body)
	}
	r = h.do("GET", p+"/files/backup?path=vars.xml&id="+hist.Rows[1].ID, admin, "")
	var b conffs.BackupContent
	r.json(t, &b)
	raw = nil
	json.Unmarshal([]byte(r.body), &raw)
	if len(raw) != 7 || b.Content != f.Content || b.ID != hist.Rows[1].ID {
		t.Fatalf("backup %.300s", r.body)
	}
	expect(t, h.do("GET", p+"/files/backup?path=vars.xml&id=nope", admin, ""), 404, `{"error":"backup not found"}`)
	expect(t, h.do("POST", p+"/files/restore", admin, `{"path":"vars.xml","id":"`+b.ID+`","baseSha256":"`+w1.SHA256+`"}`), 409, "")
	r = h.do("POST", p+"/files/restore", admin, `{"path":"vars.xml","id":"`+b.ID+`","baseSha256":"`+w2.SHA256+`"}`)
	expect(t, r, 200, "")
	if data, _ := os.ReadFile(filepath.Join(root, "vars.xml")); string(data) != f.Content {
		t.Fatal("restore")
	}

	expect(t, h.do("DELETE", p+"/files/content?path=vars.xml&baseSha256="+w2.SHA256, admin, ""), 409, "")
	expect(t, h.do("DELETE", p+"/files/content?path=vars.xml&baseSha256="+f.SHA256, admin, ""), 204, "")
	expect(t, h.do("GET", p+"/files/content?path=vars.xml", admin, ""), 404, `{"error":"file not found"}`)

	for _, q := range []string{"../vars.xml", "/etc/passwd.xml", "vars.pem", "a%00.xml"} {
		expect(t, h.do("GET", p+"/files/content?path="+q, admin, ""), 400, "")
	}
	expect(t, h.do("GET", p+"/files?path=..", admin, ""), 400, "")
	expect(t, h.do("PUT", p+"/files/content", admin, `{"path":"x.xml","content":"<a/>","baseSha256":"","extra":1}`), 400, "")

	// Backups live under BackupDir/<instance id>, outside the conf dir.
	if entries, err := os.ReadDir(filepath.Join(h.srv.cfg.BackupDir, h.id)); err != nil || len(entries) == 0 {
		t.Fatalf("backup dir: %v", err)
	}

	data := string(must(t, auditPath))
	for _, want := range []string{
		`"action":"config.write","target":"fs01","detail":"path=vars.xml base=` + f.SHA256 + `","result":"error","error":"the XML is not well-formed: element <broken> closed by </include>"`,
		`"action":"config.write","target":"fs01","detail":"path=vars.xml base=` + f.SHA256 + ` sha256=` + w1.SHA256 + `","result":"ok"`,
		`"action":"config.restore","target":"fs01","detail":"path=vars.xml id=` + b.ID + ` base=` + w2.SHA256 + ` sha256=` + f.SHA256 + `","result":"ok"`,
		`"action":"config.delete","target":"fs01","detail":"path=vars.xml base=` + f.SHA256 + `","result":"ok"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("audit lacks %s", want)
		}
	}
	if strings.Contains(data, "default_password") || strings.Contains(data, "<include") {
		t.Error("audit holds file content")
	}

	// A viewer: 403.
	expect(t, h.do("GET", p+"/files?path=", h.login("viewer", viewerPW), ""), 403, "")
}

func TestFilesAgent(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	root := confDir(t)
	l, err := conffs.NewLocal(root, filepath.Join(t.TempDir(), "agent-backups"))
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(conffs.NewAgentHandler(l, agentToken))
	defer agent.Close()
	set := func(url, token string) {
		expect(t, h.do("PUT", p, admin, `{"name":"fs01","host":"10.0.0.5","port":8021,"password":"","configMode":"agent","agentURL":"`+url+`","agentToken":"`+token+`"}`), 200, "")
	}
	set(agent.URL, agentToken)
	r := h.do("GET", p+"/files/content?path=vars.xml", admin, "")
	expect(t, r, 200, "")
	var f conffs.File
	r.json(t, &f)
	r = h.do("PUT", p+"/files/content", admin, `{"path":"vars.xml","content":"<include/>","baseSha256":"`+f.SHA256+`"}`)
	expect(t, r, 200, "")
	expect(t, h.do("PUT", p+"/files/content", admin, `{"path":"vars.xml","content":"<include/>","baseSha256":"`+f.SHA256+`"}`), 409, "")
	expect(t, h.do("PUT", p+"/files/content", admin, `{"path":"vars.xml","content":"<a>","baseSha256":""}`), 422, "")
	if hist, _ := l.History(context.Background(), "vars.xml"); len(hist) != 1 || hist[0].User != "admin" {
		t.Fatalf("agent backups %+v", hist)
	}

	// A wrong token or a dead agent is a 502, never a 401 that would look
	// like an ended session.
	set(agent.URL, "wrong-token-0123456789")
	expect(t, h.do("GET", p+"/files?path=", admin, ""), 502, `{"error":"config agent: the agent rejected the token"}`)
	agent.Close()
	set(agent.URL, agentToken)
	r = h.do("GET", p+"/files?path=", admin, "")
	expect(t, r, 502, "")
	if !strings.Contains(r.body, "cannot reach the agent") || strings.Contains(r.body, agentToken) {
		t.Fatalf("dead agent %s", r.body)
	}
}

func TestFilesSearch(t *testing.T) {
	var auditPath string
	h := newHarness(t, withBackups(t), withAudit(t, &auditPath))
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id + "/files/search"
	expect(t, h.do("GET", p+"?q=password", admin, ""), 409, `{"error":"config access is not set up for this instance"}`)
	root := confDir(t)
	expect(t, h.do("PUT", "/api/instances/"+h.id, admin, `{"name":"fs01","host":"10.0.0.5","port":8021,"password":"","configMode":"local","configPath":"`+root+`"}`), 200, "")

	expect(t, h.do("GET", p+"?q=DEFAULT_password", admin, ""), 200, `{"rows":[{"path":"vars.xml","line":1,"text":"\u003cinclude\u003e\u003cX-PRE-PROCESS cmd=\"set\" data=\"default_password=1234\"/\u003e\u003c/include\u003e"}],"truncated":false}`)
	expect(t, h.do("GET", p+"?q=DEFAULT_password&case=1", admin, ""), 200, `{"rows":[],"truncated":false}`)
	expect(t, h.do("GET", p+"?q=name&limit=1", admin, ""), 200, `{"rows":[{"path":"sip_profiles/internal.xml","line":1,"text":"\u003cprofile name=\"internal\"/\u003e"}],"truncated":false}`)
	expect(t, h.do("GET", p+"?q=e&limit=1", admin, ""), 400, `{"error":"q must be 2 to 200 characters on one line"}`)
	for q, msg := range map[string]string{
		"":                 "q must be 2 to 200 characters on one line",
		"?q=a%0Ab":         "q must be 2 to 200 characters on one line",
		"?q=ab&case=yes":   "case must be 0 or 1",
		"?q=ab&limit=0":    "limit must be a positive number",
		"?q=ab&limit=many": "limit must be a positive number",
	} {
		expect(t, h.do("GET", p+q, admin, ""), 400, `{"error":"`+msg+`"}`)
	}
	expect(t, h.do("GET", "/api/instances/0000000000000000/files/search?q=ab", admin, ""), 404, `{"error":"instance not found"}`)

	// Through the agent.
	l, _ := conffs.NewLocal(root, filepath.Join(t.TempDir(), "agent-backups"))
	agent := httptest.NewServer(conffs.NewAgentHandler(l, agentToken))
	defer agent.Close()
	expect(t, h.do("PUT", "/api/instances/"+h.id, admin, `{"name":"fs01","host":"10.0.0.5","port":8021,"password":"","configMode":"agent","agentURL":"`+agent.URL+`","agentToken":"`+agentToken+`"}`), 200, "")
	r := h.do("GET", p+"?q=profile", admin, "")
	expect(t, r, 200, `{"rows":[{"path":"sip_profiles/internal.xml","line":1,"text":"\u003cprofile name=\"internal\"/\u003e"}],"truncated":false}`)
	agent.Close()
	expect(t, h.do("GET", p+"?q=profile", admin, ""), 502, "")

	// Viewer: 403, audited as config.search; the admin's searches are not.
	expect(t, h.do("GET", p+"?q=profile", h.login("viewer", viewerPW), ""), 403, "")
	data := string(must(t, auditPath))
	if !strings.Contains(data, `"user":"viewer","ip":"127.0.0.1","action":"config.search","target":"","detail":"GET `+p+`","result":"denied"`) || strings.Contains(data, `"user":"admin","ip":"127.0.0.1","action":"config.search"`) {
		t.Fatalf("audit %s", data)
	}
}
