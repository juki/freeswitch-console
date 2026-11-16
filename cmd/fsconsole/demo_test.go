package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDemoMode starts the console with -demo and walks its main read
// routes, a call started and ended from the console, and pbx-lab, which
// never connects; then stops it and checks nothing was written.
func TestDemoMode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	dir := t.TempDir()
	cmd, sc := runMain(t, []string{"FSCONSOLE_DATA=" + dir + "/fsconsole.json", "FSCONSOLE_ADMIN_PASSWORD=an-admin-password", "FSCONSOLE_AUDIT=" + dir + "/audit.log"}, "-demo", "-listen", addr)
	var startup []string
	for sc.Scan() {
		startup = append(startup, sc.Text())
		if strings.Contains(sc.Text(), "listening on") {
			break
		}
	}
	out := strings.Join(startup, "\n")
	if !strings.Contains(out, "listening on") || !strings.Contains(out, "demo mode") || !strings.Contains(out, "FSCONSOLE_ADMIN_PASSWORD") {
		t.Fatalf("start-up output:\n%s", out)
	}
	drained := make(chan struct{})
	go func() {
		for sc.Scan() {
		}
		close(drained)
	}()

	jar, _ := cookiejar.New(nil)
	c := &testClient{t: t, base: "http://" + addr, http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
	if body := c.get("/api/version", http.StatusOK, nil); strings.TrimSpace(string(body)) != `{"demo":true,"version":"dev"}` {
		t.Fatalf("version %s", body)
	}
	var instances []struct {
		ID, Name, ConfigMode string
		State                struct {
			Connected bool
			LastError string
		}
	}
	c.get("/api/instances", http.StatusOK, &instances)
	ids := map[string]string{}
	for _, in := range instances {
		ids[in.Name] = in.ID
	}
	if len(instances) != 3 || ids["pbx-east-1"] == "" || ids["pbx-west-2"] == "" || ids["pbx-lab"] == "" {
		t.Fatalf("instances %+v", instances)
	}

	p := "/api/instances/" + ids["pbx-east-1"]
	var status struct {
		State    struct{ Connected bool }
		Hostname string
		Status   struct {
			Version  string
			Sessions struct{ Count struct{ Active int } }
		}
	}
	c.get(p+"/status", http.StatusOK, &status)
	if !status.State.Connected || status.Hostname != "pbx-east-1" || !strings.HasPrefix(status.Status.Version, "1.10.12") || status.Status.Sessions.Count.Active < 8 {
		t.Fatalf("status %+v", status)
	}
	var rows struct{ Rows []map[string]any }
	for path, min := range map[string]int{
		"/sofia": 7, "/gateways": 3, "/registrations": 14, "/channels": 8, "/calls": 4, "/modules": 40, "/vars": 30,
		"/config": 5, "/dialplan": 2, "/directory": 1, "/console/commands": 19,
	} {
		c.get(p+path, http.StatusOK, &rows)
		if len(rows.Rows) < min {
			t.Errorf("%s: %d rows, want at least %d", path, len(rows.Rows), min)
		}
	}
	for _, path := range []string{"/config/sofia.conf", "/dialplan/default", "/directory/192.0.2.11"} {
		var doc struct{ XML string }
		c.get(p+path, http.StatusOK, &doc)
		if !strings.HasPrefix(doc.XML, "<") || strings.Contains(doc.XML, "demo-only-1234") || strings.Contains(doc.XML, "demo-carrier-a") {
			t.Errorf("%s: %.200s", path, doc.XML)
		}
	}
	c.get(p+"/history", http.StatusOK, nil)

	var console struct{ Output string }
	code, body := c.do("POST", p+"/console", map[string]string{"command": "originate user/1002 &park()"})
	json.Unmarshal(body, &console)
	uuid, ok := strings.CutPrefix(strings.TrimSpace(console.Output), "+OK ")
	if code != http.StatusOK || !ok {
		t.Fatalf("originate: %d %s", code, body)
	}
	if code, body := c.do("POST", p+"/console", map[string]string{"command": "uuid_kill " + uuid}); code != http.StatusOK || !strings.Contains(string(body), "+OK") {
		t.Fatalf("uuid_kill: %d %s", code, body)
	}
	var hangups struct {
		Enabled bool
		Rows    []struct {
			UUID, Cause string
			LogLines    int
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		c.get(p+"/hangups", http.StatusOK, &hangups)
		found := false
		for _, r := range hangups.Rows {
			found = found || r.UUID == uuid && r.Cause == "NORMAL_CLEARING" && r.LogLines > 0
		}
		if found && hangups.Enabled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hang-up of %s not recorded: %+v", uuid, hangups)
		}
	}
	c.get(p+"/hangups/"+uuid, http.StatusOK, nil)

	var files struct{ Rows []struct{ Name string } }
	c.get(p+"/files?path=", http.StatusOK, &files)
	if len(files.Rows) != 6 {
		t.Fatalf("files %+v", files)
	}
	if code, body := c.do("PUT", p+"/files/content", map[string]string{"path": "vars.xml", "content": "<include/>", "baseSha256": ""}); code != http.StatusForbidden || !strings.Contains(string(body), "disabled in the demo") {
		t.Fatalf("file write: %d %s", code, body)
	}

	lab := "/api/instances/" + ids["pbx-lab"]
	var labStatus struct {
		State struct {
			Connected bool
			LastError string
		}
	}
	c.get(lab+"/status", http.StatusOK, &labStatus)
	if labStatus.State.Connected || !strings.Contains(labStatus.State.LastError, "connection refused") {
		t.Fatalf("pbx-lab %+v", labStatus)
	}
	c.get(lab+"/sofia", http.StatusServiceUnavailable, nil)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the console did not stop")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("demo mode wrote %v", entries)
	}
}
