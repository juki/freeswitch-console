package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fsdeck/internal/license"
)

func TestKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "private.key")
	now := time.Unix(1_800_000_000, 0)
	var out bytes.Buffer
	if err := run([]string{"keygen", "-out", keyFile}, &out, now); err != nil {
		t.Fatal(err)
	}
	pubLine := strings.TrimSpace(out.String())
	if !strings.HasPrefix(pubLine, "public ") || strings.Contains(out.String(), "private") {
		t.Fatalf("keygen printed %q", out.String())
	}
	pub := strings.TrimPrefix(pubLine, "public ")
	if st, _ := os.Stat(keyFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if err := run([]string{"keygen", "-out", keyFile}, &out, now); err == nil {
		t.Fatal("keygen overwrote a key")
	}
	out.Reset()
	if err := run([]string{"keygen"}, &out, now); err != nil || !strings.HasPrefix(out.String(), "private ") || !strings.Contains(out.String(), "\npublic ") {
		t.Fatalf("keygen to stdout: %v %q", err, out.String())
	}

	out.Reset()
	if err := run([]string{"sign", "-key", keyFile, "-licensee", "Acme Ltd", "-plan", "pro", "-features", "users, audit,users", "-days", "30", "-id", "L-1"}, &out, now); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(out.String())
	pk, _ := license.ParsePublicKey(pub)
	k := license.Check(key, pk)
	l, _ := k.License()
	if k.Status(now) != "valid" || l.ID != "L-1" || l.Licensee != "Acme Ltd" || l.Plan != "pro" || strings.Join(l.Features, ",") != "users,audit" || l.Issued != now.Unix() || l.Expires != now.Add(30*24*time.Hour).Unix() {
		t.Fatalf("licence %+v", l)
	}

	keyPath := filepath.Join(dir, "l.key")
	os.WriteFile(keyPath, []byte(key+"\n"), 0o600)
	for _, arg := range []string{key, keyPath} {
		out.Reset()
		if err := run([]string{"verify", "-pub", pub, arg}, &out, now); err != nil || !strings.Contains(out.String(), "status valid") || !strings.Contains(out.String(), `"licensee": "Acme Ltd"`) {
			t.Fatalf("verify %v %q", err, out.String())
		}
	}
	out.Reset()
	if err := run([]string{"verify", "-pub", pub, key}, &out, now.Add(31*24*time.Hour)); err == nil || !strings.Contains(out.String(), "status expired") {
		t.Fatalf("expired: %v %q", err, out.String())
	}
	if err := run([]string{"verify", key}, &out, now); err == nil {
		t.Fatal("verified against the embedded key")
	}

	for _, args := range [][]string{
		{},
		{"nope"},
		{"sign", "-key", keyFile, "-features", "users"},
		{"sign", "-key", keyFile, "-licensee", "x", "-features", "teleport"},
		{"sign", "-key", keyFile, "-licensee", "x", "-features", ""},
		{"sign", "-key", keyFile, "-licensee", "x", "-features", "users", "-days", "-1"},
		{"sign", "-key", filepath.Join(dir, "missing"), "-licensee", "x", "-features", "users"},
		{"sign", "-key", keyPath, "-licensee", "x", "-features", "users"},
		{"verify"},
		{"verify", "-pub", "bad", key},
	} {
		if err := run(args, &out, now); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}
