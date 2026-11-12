//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fsdeck/ee/conffs"
	"fsdeck/internal/license"
	"fsdeck/internal/server"
)

// withLicence signs a licence for every feature with a key made for the
// test; the development key is never needed.
func withLicence(t *testing.T) func(*server.Config) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := license.Sign(license.License{ID: "e2e", Licensee: "e2e", Plan: "test", Features: license.Features, Issued: time.Now().Unix()}, priv)
	if err != nil {
		t.Fatal(err)
	}
	return func(c *server.Config) { c.License = license.Check(key, pub) }
}

func confTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "freeswitch")
	os.MkdirAll(filepath.Join(root, "sip_profiles"), 0o755)
	os.WriteFile(filepath.Join(root, "vars.xml"), []byte("<include>\n  <X-PRE-PROCESS cmd=\"set\" data=\"domain=$${local_ip_v4}\"/>\n</include>\n"), 0o644)
	return root
}

// TestEndToEndPaid covers the paid features through the real wiring: the
// operator role, the audit query, config editing in local and agent mode,
// and the 402 answers without a licence.
func TestEndToEndPaid(t *testing.T) {
	sk := startStack(t, withLicence(t))
	c := sk.client(t, "admin", "admin-pw")

	var lic server.LicenseInfo
	c.get("/api/license", http.StatusOK, &lic)
	if lic.Edition != "enterprise" || lic.Status != "valid" || len(lic.Features) != 3 || lic.Expires != nil {
		t.Fatalf("licence %+v", lic)
	}

	root := confTree(t)
	code, body := c.do("POST", "/api/instances", map[string]any{"name": "test", "host": sk.host, "port": sk.port, "password": eslPassword, "configMode": "local", "configPath": root})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var inst struct{ ID, ConfigMode, ConfigPath string }
	json.Unmarshal(body, &inst)
	if inst.ConfigMode != "local" || inst.ConfigPath != root {
		t.Fatalf("instance %s", body)
	}
	p := "/api/instances/" + inst.ID

	// Local mode: read, write, 409 on a stale hash, 422 on broken XML,
	// history, restore.
	var f conffs.File
	c.get(p+"/files/content?path=vars.xml", http.StatusOK, &f)
	write := func(content, base string) (int, []byte) {
		return c.do("PUT", p+"/files/content", map[string]string{"path": "vars.xml", "content": content, "baseSha256": base})
	}
	edited := strings.Replace(f.Content, "local_ip_v4", "local_ip_v6", 1)
	code, body = write(edited, f.SHA256)
	var w conffs.Written
	if code != http.StatusOK || json.Unmarshal(body, &w) != nil || w.Size != int64(len(edited)) {
		t.Fatalf("write: %d %s", code, body)
	}
	if code, body = write("<include/>", f.SHA256); code != http.StatusConflict || !bytes.Contains(body, []byte(`"currentSha256":"`+w.SHA256+`"`)) {
		t.Fatalf("stale write: %d %s", code, body)
	}
	if code, body = write("<include>\n<oops>\n</include>\n", w.SHA256); code != http.StatusUnprocessableEntity || !bytes.Contains(body, []byte(`"line":3`)) {
		t.Fatalf("broken XML: %d %s", code, body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "vars.xml")); string(data) != edited {
		t.Fatalf("file on disk: %s", data)
	}
	var hist struct{ Rows []conffs.Backup }
	c.get(p+"/files/history?path=vars.xml", http.StatusOK, &hist)
	if len(hist.Rows) != 1 || hist.Rows[0].SHA256 != f.SHA256 || hist.Rows[0].User != "admin" {
		t.Fatalf("history %+v", hist)
	}
	code, body = c.do("POST", p+"/files/restore", map[string]string{"path": "vars.xml", "id": hist.Rows[0].ID, "baseSha256": w.SHA256})
	if code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "vars.xml")); string(data) != f.Content {
		t.Fatalf("restored file: %s", data)
	}
	if _, err := os.Stat(filepath.Join(sk.dir, "backups", inst.ID)); err != nil {
		t.Fatalf("backups not beside the data file: %v", err)
	}

	// The audit query is licensed: the config changes are in it.
	var audit struct {
		Rows []struct{ Action, Target, Result string }
	}
	c.get("/api/audit?q=config.&limit=10", http.StatusOK, &audit)
	if len(audit.Rows) != 4 || audit.Rows[0].Action != "config.restore" || audit.Rows[3].Action != "config.write" || audit.Rows[3].Target != "test" {
		t.Fatalf("audit %+v", audit.Rows)
	}

	// Agent mode: the agent handler on a loopback listener, its own backups.
	agentRoot := confTree(t)
	store, err := conffs.NewLocal(agentRoot, filepath.Join(t.TempDir(), "agent-backups"))
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(conffs.NewAgentHandler(store, "e2e-agent-token-1234"))
	defer agent.Close()
	code, body = c.do("PUT", p, map[string]any{"name": "test", "host": sk.host, "port": sk.port, "password": "", "configMode": "agent", "agentURL": agent.URL, "agentToken": "e2e-agent-token-1234"})
	if code != http.StatusOK || bytes.Contains(body, []byte("e2e-agent-token")) {
		t.Fatalf("switch to agent: %d %s", code, body)
	}
	c.get(p+"/files/content?path=vars.xml", http.StatusOK, &f)
	if code, body = write("<include/>\n", f.SHA256); code != http.StatusOK {
		t.Fatalf("write through the agent: %d %s", code, body)
	}
	if data, _ := os.ReadFile(filepath.Join(agentRoot, "vars.xml")); string(data) != "<include/>\n" {
		t.Fatalf("agent file: %s", data)
	}
	c.get(p+"/files/history?path=vars.xml", http.StatusOK, &hist)
	if len(hist.Rows) != 1 {
		t.Fatalf("agent history %+v", hist)
	}

	// An operator: hang up and rescan yes; console, admin operations, user
	// management and config files no.
	code, body = c.do("POST", "/api/users", map[string]string{"username": "op1", "role": "operator", "password": "operator-password"})
	if code != http.StatusCreated || bytes.Contains(body, []byte("operator-password")) {
		t.Fatalf("create operator: %d %s", code, body)
	}
	o := sk.client(t, "op1", "operator-password")
	if code, body := o.do("DELETE", p+"/channels/abc-123", nil); code != http.StatusOK {
		t.Fatalf("operator hangup: %d %s", code, body)
	}
	if code, _ := o.do("POST", p+"/actions", map[string]string{"action": "reloadxml"}); code == http.StatusForbidden {
		t.Fatal("operator reloadxml refused")
	}
	for _, rq := range []struct {
		method, path string
		body         any
	}{
		{"POST", p + "/console", map[string]string{"command": "status"}},
		{"POST", p + "/actions", map[string]any{"action": "fsctl", "op": "loglevel", "value": 6}},
		{"GET", "/api/users", nil},
		{"POST", "/api/users", map[string]string{"username": "op2", "role": "admin", "password": "operator-password"}},
		{"GET", "/api/audit", nil},
		{"GET", p + "/files?path=", nil},
		{"POST", "/api/instances", map[string]any{"name": "x", "host": "h", "port": 1, "password": "p"}},
	} {
		if code, body := o.do(rq.method, rq.path, rq.body); code != http.StatusForbidden {
			t.Fatalf("operator %s %s: %d %s", rq.method, rq.path, code, body)
		}
	}

	// Without a licence the same routes answer 402.
	free := startStack(t, nil)
	fc := free.client(t, "admin", "admin-pw")
	code, body = fc.do("POST", "/api/instances", map[string]any{"name": "t", "host": free.host, "port": free.port, "password": eslPassword, "configMode": "local", "configPath": root})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	json.Unmarshal(body, &inst)
	fp := "/api/instances/" + inst.ID + "/files"
	for _, rq := range []struct {
		method, path string
		body         any
	}{
		{"GET", fp + "?path=", nil},
		{"GET", fp + "/content?path=vars.xml", nil},
		{"PUT", fp + "/content", map[string]string{"path": "vars.xml", "content": "<a/>", "baseSha256": ""}},
		{"DELETE", fp + "/content?path=vars.xml&baseSha256=x", nil},
		{"GET", fp + "/history?path=vars.xml", nil},
		{"GET", fp + "/backup?path=vars.xml&id=x", nil},
		{"POST", fp + "/restore", map[string]string{"path": "vars.xml", "id": "x", "baseSha256": ""}},
		{"GET", "/api/audit", nil},
		{"POST", "/api/users", map[string]string{"username": "op3", "role": "viewer", "password": "operator-password"}},
	} {
		code, body := fc.do(rq.method, rq.path, rq.body)
		if code != http.StatusPaymentRequired || !bytes.Contains(body, []byte(`"error":"this feature needs a licence"`)) {
			t.Fatalf("unlicensed %s %s: %d %s", rq.method, rq.path, code, body)
		}
	}
}
