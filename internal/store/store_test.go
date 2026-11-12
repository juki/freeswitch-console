package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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

const testKey = "a-secret-key-of-32-characters-xx"

func TestEncryptedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s, err := Open(path, WithSecretKey(testKey))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Add(Instance{Name: "a", Host: "h", Port: 1, Password: "ClueCon-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Instance{Name: "b", Host: "h", Port: 2}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(a.ID); got.Password != "ClueCon-secret" {
		t.Fatalf("in memory %q", got.Password)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "ClueCon-secret") || strings.Count(string(data), `"password": "enc:v1:`) != 2 {
		t.Fatalf("file %s", data)
	}
	r, err := Open(path, WithSecretKey(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if list := r.List(); list[0].Password != "ClueCon-secret" || list[1].Password != "" {
		t.Fatalf("reopened %+v", list)
	}
	// A fresh nonce per save: the same value encrypts differently.
	if err := r.Update(Instance{ID: a.ID, Name: "a", Host: "h", Port: 1}); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) == string(data) {
		t.Fatal("ciphertext reused")
	}

	if _, err := Open(path); !errors.Is(err, ErrSecretKeyMissing) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := Open(path, WithSecretKey("another-key-of-sufficient-length")); !errors.Is(err, ErrSecretKeyWrong) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := Open(path, WithSecretKey("short-key-15chr")); !errors.Is(err, ErrSecretKeyShort) {
		t.Fatalf("short key: %v", err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "new.json"), WithSecretKey("ünïcödé-sixteen!")); err != nil {
		t.Fatalf("16 runes: %v", err)
	}
	for _, bad := range []string{"enc:v1:!!!", "enc:v1:AAAA"} {
		os.WriteFile(path, []byte(`{"instances":[{"id":"x","name":"a","host":"h","port":1,"password":"`+bad+`"}]}`), 0o600)
		if _, err := Open(path, WithSecretKey(testKey)); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}

func TestPlainMigratesOnNextSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	legacy := `{"instances":[{"id":"0123456789abcdef","name":"a","host":"h","port":8021,"password":"ClueCon"}]}`
	os.WriteFile(path, []byte(legacy), 0o600)
	s, err := Open(path, WithSecretKey(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.List(); got[0].Password != "ClueCon" {
		t.Fatalf("plain value read as %q", got[0].Password)
	}
	if data, _ := os.ReadFile(path); string(data) != legacy {
		t.Fatal("file rewritten before any change")
	}
	if _, err := s.Add(Instance{Name: "b", Host: "h", Port: 1, Password: "x"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "ClueCon") || strings.Count(string(data), "enc:v1:") != 2 {
		t.Fatalf("not migrated: %s", data)
	}
	r, err := Open(path, WithSecretKey(testKey))
	if err != nil || r.List()[0].Password != "ClueCon" {
		t.Fatalf("reopen: %v %+v", err, r.List())
	}
}

func TestLegacyFileLoadsAndKeepsShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(path, []byte(`{"instances":[{"id":"0123456789abcdef","name":"a","host":"h","port":8021,"password":"ClueCon"}]}`), 0o600)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Users()) != 0 || len(s.List()) != 1 {
		t.Fatalf("loaded %+v %+v", s.List(), s.Users())
	}
	if _, err := s.Add(Instance{Name: "b", Host: "h", Port: 1}); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || raw["instances"] == nil || strings.Contains(string(data), "enc:") {
		t.Fatalf("shape changed: %s", data)
	}
}

func TestUsers(t *testing.T) {
	s, path := openTemp(t)
	inst, _ := s.Add(Instance{Name: "a", Host: "h", Port: 1, Password: "p"})
	u, err := s.AddUser(User{ID: "ignored", Username: "alice", Role: RoleOperator, PasswordHash: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(u.ID) || u.CreatedAt.IsZero() || u.CreatedAt.Location() != time.UTC {
		t.Fatalf("added %+v", u)
	}
	if _, err := s.AddUser(User{Username: "alice", Role: RoleViewer, PasswordHash: "h"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	b, _ := s.AddUser(User{Username: "bob", Role: RoleAdmin, PasswordHash: "h2"})
	for _, bad := range []User{
		{Username: "a", Role: RoleAdmin, PasswordHash: "h"},
		{Username: "Alice", Role: RoleAdmin, PasswordHash: "h"},
		{Username: "-alice", Role: RoleAdmin, PasswordHash: "h"},
		{Username: "al ice", Role: RoleAdmin, PasswordHash: "h"},
		{Username: strings.Repeat("a", 33), Role: RoleAdmin, PasswordHash: "h"},
		{Username: "carol", Role: "root", PasswordHash: "h"},
		{Username: "carol", Role: RoleAdmin},
	} {
		if _, err := s.AddUser(bad); !errors.Is(err, ErrInvalidUser) {
			t.Errorf("AddUser(%+v) = %v", bad, err)
		}
	}
	for _, good := range []string{"a1", "0.x", "x_y-z.w", strings.Repeat("a", 32)} {
		if CheckUsername(good) != nil {
			t.Errorf("%q rejected", good)
		}
	}

	u.Role, u.Disabled, u.PasswordHash, u.CreatedAt = RoleViewer, true, "h3", time.Time{}
	if err := s.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(User{ID: b.ID, Username: "alice", Role: RoleAdmin, PasswordHash: "h"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("rename onto another: %v", err)
	}
	if err := s.UpdateUser(User{ID: "nope", Username: "x1", Role: RoleAdmin, PasswordHash: "h"}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("update unknown: %v", err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.UserByName("alice")
	if !ok || got.Role != RoleViewer || !got.Disabled || got.PasswordHash != "h3" || got.CreatedAt.IsZero() || got.ID != u.ID {
		t.Fatalf("reopened %+v", got)
	}
	if got, ok := r.User(b.ID); !ok || got.Username != "bob" {
		t.Fatalf("by id %+v", got)
	}
	if list := r.List(); len(list) != 1 || list[0].ID != inst.ID {
		t.Fatalf("instances %+v", list)
	}
	if err := r.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteUser(u.ID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, ok := r.UserByName("alice"); ok {
		t.Fatal("deleted user found")
	}
	// Instance changes keep the users.
	r.Delete(inst.ID)
	if r2, _ := Open(path); len(r2.Users()) != 1 {
		t.Fatalf("users after instance delete: %+v", r2.Users())
	}
}

func TestUsersInFileAreValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	for _, users := range []string{
		`[{"id":"1","username":"Bad Name","role":"admin","passwordHash":"h"}]`,
		`[{"id":"1","username":"alice","role":"root","passwordHash":"h"}]`,
		`[{"id":"1","username":"alice","role":"admin","passwordHash":"h"},{"id":"2","username":"alice","role":"admin","passwordHash":"h"}]`,
	} {
		os.WriteFile(path, []byte(`{"instances":[],"users":`+users+`}`), 0o600)
		if _, err := Open(path); err == nil {
			t.Errorf("opened %s", users)
		}
	}
}
