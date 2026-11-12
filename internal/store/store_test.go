package store

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "instances.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestRoundTripAndReopen(t *testing.T) {
	s, path := openTemp(t)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("new store lists %v", got)
	}
	a, err := s.Add(Instance{ID: "ignored", Name: "fs-a", Host: "10.0.0.1", Port: 8021, Password: "ClueCon"})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a.ID) {
		t.Fatalf("id %q", a.ID)
	}
	b, err := s.Add(Instance{Name: "fs-b", Host: "fs-b.example.com", Port: 8022, Password: "pw-b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("duplicate id")
	}

	// Empty password keeps the old one.
	if err := s.Update(Instance{ID: a.ID, Name: "fs-a2", Host: "10.0.0.2", Port: 8021}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(a.ID)
	if !ok || got.Name != "fs-a2" || got.Host != "10.0.0.2" || got.Password != "ClueCon" {
		t.Fatalf("after update %+v", got)
	}
	if err := s.Update(Instance{ID: b.ID, Name: "fs-b", Host: "fs-b.example.com", Port: 8022, Password: "new"}); err != nil {
		t.Fatal(err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	list := r.List()
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID || list[1].Password != "new" || list[0].Password != "ClueCon" {
		t.Fatalf("reopened %+v", list)
	}

	if err := r.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(a.ID); ok {
		t.Fatal("deleted instance still present")
	}
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if list := r2.List(); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("after delete %+v", list)
	}
}

func TestListIsCopy(t *testing.T) {
	s, _ := openTemp(t)
	a, _ := s.Add(Instance{Name: "a", Host: "h", Port: 1})
	list := s.List()
	list[0].Name = "changed"
	if got, _ := s.Get(a.ID); got.Name != "a" {
		t.Fatal("List exposes internal state")
	}
}

func TestFileModeAndNoTempLeft(t *testing.T) {
	s, path := openTemp(t)
	if err := os.WriteFile(path, []byte(`{"instances":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Instance{Name: "a", Host: "h", Port: 1}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries", len(entries))
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "{\n  \"instances\": [") {
		t.Fatalf("file %s", data)
	}
}

func TestValidation(t *testing.T) {
	s, _ := openTemp(t)
	bad := []Instance{
		{Name: "", Host: "h", Port: 1},
		{Name: strings.Repeat("x", 65), Host: "h", Port: 1},
		{Name: "a", Host: "", Port: 1},
		{Name: "a", Host: "h h", Port: 1},
		{Name: "a", Host: "h\n", Port: 1},
		{Name: "a", Host: "h", Port: 0},
		{Name: "a", Host: "h", Port: 65536},
	}
	for _, in := range bad {
		if _, err := s.Add(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Add(%+v) = %v", in, err)
		}
	}
	if _, err := s.Add(Instance{Name: strings.Repeat("é", 64), Host: "::1", Port: 65535}); err != nil {
		t.Errorf("64 runes: %v", err)
	}
	if len(s.List()) != 1 {
		t.Fatalf("invalid instances stored: %v", s.List())
	}
	a := s.List()[0]
	a.Port = 0
	if err := s.Update(a); !errors.Is(err, ErrInvalid) {
		t.Errorf("update invalid: %v", err)
	}
	if err := s.Update(Instance{ID: "nope", Name: "a", Host: "h", Port: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}
	if err := s.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("{"), 0o600)
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt file opened")
	}
}

func TestSaveFailureKeepsState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "missing-dir", "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Instance{Name: "a", Host: "h", Port: 1}); err == nil {
		t.Fatal("save into missing directory succeeded")
	}
	if len(s.List()) != 0 {
		t.Fatal("failed add is visible")
	}
}

func TestConcurrent(t *testing.T) {
	s, path := openTemp(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := s.Add(Instance{Name: "n", Host: "h", Port: 1})
			if err != nil {
				t.Error(err)
				return
			}
			s.List()
			s.Update(Instance{ID: in.ID, Name: "m", Host: "h", Port: 2})
		}()
	}
	wg.Wait()
	r, _ := Open(path)
	if len(r.List()) != 20 {
		t.Fatalf("got %d instances", len(r.List()))
	}
}
