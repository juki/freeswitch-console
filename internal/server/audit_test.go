package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fsdeck/internal/audit"
)

// withAudit gives the harness an audit log; its path is returned through
// *path.
func withAudit(t *testing.T, path *string) func(*Config) {
	return func(c *Config) {
		*path = filepath.Join(t.TempDir(), "fsdeck-audit.log")
		l, err := audit.Open(*path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		c.Audit = l
	}
}

func readAudit(t *testing.T, path string) []audit.Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestAuditWriteFailureKeepsTheAction(t *testing.T) {
	var path string
	h := newHarness(t, withAudit(t, &path))
	admin := h.login("admin", adminPW)
	h.srv.cfg.Audit.Close()
	os.RemoveAll(filepath.Dir(path))
	expect(t, h.do("POST", "/api/instances/"+h.id+"/console", admin, `{"command":"status"}`), 200, "")
	expect(t, h.do("POST", "/api/logout", admin, ""), 204, "")
}
