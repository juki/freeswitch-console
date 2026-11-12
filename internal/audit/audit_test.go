package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func TestWriteAndRead(t *testing.T) {
	l, path := openTemp(t)
	before := time.Now().UTC().Truncate(time.Second)
	l.Write(Entry{User: "admin", IP: "192.0.2.1", Action: "console.run", Target: "fs01", Detail: "show <calls> & \"x\"", Result: OK})
	l.Write(Entry{Time: "2026-01-02T03:04:05Z", Action: "login.failed", Result: Denied})
	l.Write(Entry{User: "Bob", Action: "user.create", Target: "ALICE", Result: Error, Error: "invalid user"})

	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("file %q", data)
	}
	// Every key is present, in order, and HTML characters are not escaped.
	want := `{"time":"2026-01-02T03:04:05Z","user":"","ip":"","action":"login.failed","target":"","detail":"","result":"denied","error":""}`
	if lines[1] != want {
		t.Fatalf("line\n got %s\nwant %s", lines[1], want)
	}
	if !strings.Contains(lines[0], `"detail":"show <calls> & \"x\""`) {
		t.Fatalf("line %s", lines[0])
	}
	var first Entry
	json.Unmarshal([]byte(lines[0]), &first)
	ts, err := time.Parse(time.RFC3339, first.Time)
	if err != nil || ts.Before(before) || ts.Location() != time.UTC || !strings.HasSuffix(first.Time, "Z") {
		t.Fatalf("time %q: %v", first.Time, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}

	rows, err := l.Read(10, "")
	if err != nil || len(rows) != 3 || rows[0].Action != "user.create" || rows[2].Action != "console.run" {
		t.Fatalf("rows %+v %v", rows, err)
	}
	if rows, _ := l.Read(2, ""); len(rows) != 2 || rows[1].Action != "login.failed" {
		t.Fatalf("limit: %+v", rows)
	}
	for q, n := range map[string]int{"alice": 1, "BOB": 1, "<CALLS>": 1, "denied": 1, "2026-01-02": 1, "user": 1, "nothing": 0, "": 3} {
		if rows, _ := l.Read(10, q); len(rows) != n {
			t.Errorf("q %q: %d rows", q, len(rows))
		}
	}
}

func TestReopenAppends(t *testing.T) {
	l, path := openTemp(t)
	l.Write(Entry{Action: "a"})
	l.Close()
	l.Write(Entry{Action: "b"}) // a write after Close reopens
	l.Close()
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Write(Entry{Action: "c"})
	rows, _ := m.Read(10, "")
	if len(rows) != 3 || rows[0].Action != "c" || rows[2].Action != "a" {
		t.Fatalf("rows %+v", rows)
	}
}

func TestRotation(t *testing.T) {
	l, path := openTemp(t)
	l.maxSize = 1000
	for i := 0; i < 30; i++ {
		l.Write(Entry{Action: "x", Detail: fmt.Sprintf("n=%02d", i)})
	}
	cur, _ := os.Stat(path)
	old, err := os.Stat(path + ".1")
	if err != nil || cur.Size() > 1000 || old.Size() > 1000 || cur.Size() == 0 {
		t.Fatalf("sizes %v %v %v", cur.Size(), old, err)
	}
	// Only one older file is kept; the newest rows come from the current
	// file, then from .1.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Fatalf("%d files", len(entries))
	}
	rows, _ := l.Read(1000, "")
	if rows[0].Detail != "n=29" {
		t.Fatalf("newest %+v", rows[0])
	}
	for i := 1; i < len(rows); i++ {
		var a, b int
		fmt.Sscanf(rows[i-1].Detail, "n=%d", &a)
		fmt.Sscanf(rows[i].Detail, "n=%d", &b)
		if b != a-1 {
			t.Fatalf("row %d: %s after %s", i, rows[i].Detail, rows[i-1].Detail)
		}
	}
	if len(rows) < 10 || len(rows) >= 30 {
		t.Fatalf("%d rows across two files", len(rows))
	}
	// A limit served by the current file alone does not need .1.
	n := strings.Count(string(must(os.ReadFile(path))), "\n")
	if rows, _ := l.Read(n, ""); len(rows) != n || rows[n-1].Detail == "" {
		t.Fatalf("current file only: %d", len(rows))
	}
	if rows, _ := l.Read(n+1, ""); len(rows) != n+1 {
		t.Fatalf("spill into .1: %d", len(rows))
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestSkipsBrokenLines(t *testing.T) {
	l, path := openTemp(t)
	l.Write(Entry{Action: "a"})
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("not json\n{\"action\":\"partial")
	f.Close()
	l.Close()
	l2, _ := Open(path)
	defer l2.Close()
	rows, err := l2.Read(10, "")
	if err != nil || len(rows) != 1 || rows[0].Action != "a" {
		t.Fatalf("rows %+v %v", rows, err)
	}
}

func TestDisabled(t *testing.T) {
	var l *Log
	l.Write(Entry{Action: "x"})
	rows, err := l.Read(10, "")
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("rows %v %v", rows, err)
	}
	if l.Close() != nil {
		t.Fatal("close")
	}
}

func TestWriteFailureIsLoggedNotReturned(t *testing.T) {
	var stderr bytes.Buffer
	log.SetOutput(&stderr)
	defer log.SetOutput(os.Stderr)
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	os.RemoveAll(dir) // the reopen fails
	l.Write(Entry{Action: "console.run", Detail: "secret-command"})
	if !strings.Contains(stderr.String(), "audit:") || strings.Contains(stderr.String(), "secret-command") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if _, err := Open(filepath.Join(dir, "x", "audit.log")); err == nil {
		t.Fatal("open in a missing directory succeeded")
	}
}

func TestConcurrentWrites(t *testing.T) {
	l, path := openTemp(t)
	l.maxSize = 4000
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l.Write(Entry{Action: "x", Detail: strings.Repeat("y", 20)})
				if i%10 == 0 {
					l.Read(5, "x")
				}
			}
		}()
	}
	wg.Wait()
	// Every line in both files is whole.
	for _, p := range []string{path, path + ".1"} {
		data, _ := os.ReadFile(p)
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			var e Entry
			if err := json.Unmarshal([]byte(line), &e); err != nil || e.Action != "x" {
				t.Fatalf("%s: broken line %q", p, line)
			}
		}
	}
}
