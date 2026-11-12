//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetup(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "conf")
	os.Mkdir(root, 0o755)
	os.WriteFile(filepath.Join(root, "vars.xml"), []byte("<include/>"), 0o644)
	backups := filepath.Join(dir, "backups")
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("agent-token-0123456789\n"), 0o600)
	short := filepath.Join(dir, "short")
	os.WriteFile(short, []byte("short\n"), 0o600)
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, nil, 0o600)
	open := filepath.Join(dir, "open")
	os.WriteFile(open, []byte("agent-token-0123456789"), 0o644)

	base := []string{"-root", root, "-backups", backups}
	for name, args := range map[string][]string{
		"no token":         base,
		"missing token":    append(base, "-token-file", filepath.Join(dir, "nope")),
		"empty token":      append(base, "-token-file", empty),
		"short token":      append(base, "-token-file", short),
		"missing root":     {"-root", filepath.Join(dir, "x"), "-backups", backups, "-token-file", token},
		"backups in root":  {"-root", root, "-backups", filepath.Join(root, "b"), "-token-file", token},
		"cert without key": append(base, "-token-file", token, "-tls-cert", token),
		"bad cert":         append(base, "-token-file", token, "-tls-cert", token, "-tls-key", token),
		"unknown flag":     append(base, "-token-file", token, "-x"),
	} {
		if _, err := setup(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: started", name)
		}
	}

	var stderr bytes.Buffer
	if _, err := setup(append(base, "-token-file", open), &stderr); err != nil || !strings.Contains(stderr.String(), "chmod 600") {
		t.Fatalf("readable token file: %v %q", err, stderr.String())
	}

	srv, err := setup(append(base, "-token-file", token, "-listen", "127.0.0.1:0"), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	for auth, code := range map[string]int{"": 401, "Bearer agent-token-0123456789": 200} {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/files/content?path=vars.xml", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%q: %d", auth, resp.StatusCode)
		}
	}
}
