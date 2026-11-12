//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package conffs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

// fixture is a root with a few files, a secret beside it and backups
// elsewhere.
type fixture struct {
	base, root, backups string
	l                   *Local
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	f := fixture{base: base, root: filepath.Join(base, "conf"), backups: filepath.Join(base, "data", "backups", "inst1")}
	for _, d := range []string{"sip_profiles/external", "autoload_configs", "real"} {
		os.MkdirAll(filepath.Join(f.root, d), 0o755)
	}
	write := func(p, s string, mode os.FileMode) {
		os.WriteFile(filepath.Join(f.root, p), []byte(s), mode)
	}
	write("vars.xml", `<include><X-PRE-PROCESS cmd="set" data="domain=$${local_ip_v4}"/></include>`, 0o640)
	write("sip_profiles/internal.xml", `<profile name="internal"><settings><param name="sip-port" value="$${internal_sip_port}"/></settings></profile>`, 0o644)
	write("real/target.xml", `<include/>`, 0o644)
	write("binary.xml", "\xff\xfe<x/>", 0o644)
	write("notes.pem", "-----BEGIN", 0o600)
	os.WriteFile(filepath.Join(base, "secret.xml"), []byte(`<secret/>`), 0o600)
	os.Symlink("../secret.xml", filepath.Join(f.root, "out.xml"))
	os.Symlink(filepath.Join(base, "secret.xml"), filepath.Join(f.root, "outabs.xml"))
	os.Symlink(base, filepath.Join(f.root, "updir"))
	os.Symlink("real", filepath.Join(f.root, "linkdir"))
	os.Symlink("real/target.xml", filepath.Join(f.root, "inside.xml"))
	l, err := NewLocal(f.root, f.backups)
	if err != nil {
		t.Fatal(err)
	}
	f.l = l
	return f
}

func isInvalid(err error) bool { return errors.Is(err, ErrInvalid) }

func TestCleanPath(t *testing.T) {
	for p, want := range map[string]string{
		"vars.xml":                    "vars.xml",
		"./vars.xml":                  "vars.xml",
		"sip_profiles//internal.xml":  "sip_profiles/internal.xml",
		"sip_profiles/./internal.xml": "sip_profiles/internal.xml",
		"a/b.LUA":                     "a/b.LUA",
		"%2e%2e/x.xml":                "%2e%2e/x.xml", // already decoded once: a literal name
	} {
		if got, err := CleanPath(p, true); err != nil || got != want {
			t.Errorf("%q: %q %v", p, got, err)
		}
	}
	for _, p := range []string{
		"", ".", "/etc/passwd.xml", "../x.xml", "a/../../x.xml", "a/../x.xml", "..",
		"a\x00.xml", "a\n.xml", "a\\..\\x.xml", "C:\\x.xml", "x.pem", "x", "x.xml.bak", "dir/",
		strings.Repeat("a/", 600) + "x.xml",
	} {
		if _, err := CleanPath(p, true); !isInvalid(err) {
			t.Errorf("%q accepted: %v", p, err)
		}
	}
	for p, want := range map[string]string{"": ".", ".": ".", "sip_profiles/": "sip_profiles", "./a/b": "a/b"} {
		if got, err := CleanPath(p, false); err != nil || got != want {
			t.Errorf("dir %q: %q %v", p, got, err)
		}
	}
	for _, p := range []string{"/", "..", "a/..", "/etc"} {
		if _, err := CleanPath(p, false); !isInvalid(err) {
			t.Errorf("dir %q accepted", p)
		}
	}
	for _, ext := range Extensions {
		if _, err := CleanPath("f"+strings.ToUpper(ext), true); err != nil {
			t.Errorf("%s: %v", ext, err)
		}
	}
}

func TestCheckXML(t *testing.T) {
	good := []string{
		"",
		"  \n",
		`<?xml version="1.0" encoding="ISO-8859-1"?><include/>`,
		`<include>
  <!-- comment with $${var} -->
  <X-PRE-PROCESS cmd="include" data="sip_profiles/*.xml"/>
  <X-PRE-PROCESS cmd="set" data="default_password=1234"/>
  <user id="1000"><params><param name="password" value="$${default_password}"/></params></user>
</include>`,
		`<configuration name="lua.conf" description="LUA"><settings><param name="script-directory" value="$${script_dir}/?.lua"/></settings></configuration>`,
		`<extension name="x"><condition field="destination_number" expression="^(\d{4})$"><action application="bridge" data="{ignore_early_media=true}user/$1@${domain_name}"/></condition></extension>`,
		`<include><![CDATA[ <not> & parsed ]]></include>`,
		"<a>caf\u00e9 &amp; &lt;b&gt;</a>",
	}
	for _, s := range good {
		if err := CheckXML(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := map[string]int{
		"<include>\n  <param name=\"x\">\n</include>": 3,
		"<include>\n<a></b>":                          2,
		"<include>\n\n":                               3,
		"<a>&nbsp;</a>":                               1,
		"<a>\n & </a>":                                2,
		"<a x=unquoted/>":                             1,
	}
	for s, line := range bad {
		var se *SyntaxError
		if err := CheckXML(s); !errors.As(err, &se) || se.Line != line || se.Msg == "" {
			t.Errorf("%q: %v (line %v)", s, err, se)
		}
	}
}

func TestLocalTraversal(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{
		"../secret.xml",
		"/etc/hosts.xml",
		"out.xml",              // relative link out of the root
		"outabs.xml",           // absolute link out of the root
		"updir/secret.xml",     // link in the middle of the path
		"updir/conf/vars.xml",  // back in through a link that left
		"sub/../../secret.xml", // rejected before the file system
	} {
		_, err := f.l.Read(ctx, p)
		if err == nil || (!isInvalid(err) && !errors.Is(err, ErrNotFound)) {
			t.Errorf("read %q: %v", p, err)
		}
		if _, err := f.l.Write(ctx, p, "<x/>", "", "u"); err == nil {
			t.Errorf("write %q succeeded", p)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(f.base, "secret.xml")); string(data) != `<secret/>` {
		t.Fatal("secret changed")
	}
	// An encoded traversal arrives decoded once at most: what is left is a
	// literal name inside the root.
	if _, err := f.l.Write(ctx, "..%2fsecret.xml", "<x/>", "", "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "..%2fsecret.xml")); err != nil {
		t.Fatal("literal name not inside the root")
	}
	for _, d := range []string{"updir", "../", "/"} {
		if _, err := f.l.List(ctx, d); err == nil {
			t.Errorf("list %q succeeded", d)
		}
	}
	// Links that stay inside the root are followed for reading.
	if got, err := f.l.Read(ctx, "linkdir/target.xml"); err != nil || got.Content != `<include/>` || got.Path != "linkdir/target.xml" {
		t.Fatalf("read through an inside link: %+v %v", got, err)
	}
	if got, err := f.l.Read(ctx, "inside.xml"); err != nil || got.Content != `<include/>` {
		t.Fatalf("read an inside link: %v", err)
	}
	// ...but a write or delete never goes through a final link.
	inside, _ := f.l.Read(ctx, "inside.xml")
	if _, err := f.l.Write(ctx, "inside.xml", "<x/>", inside.SHA256, "u"); !isInvalid(err) {
		t.Fatalf("write through a link: %v", err)
	}
	if err := f.l.Delete(ctx, "inside.xml", inside.SHA256, "u"); !isInvalid(err) {
		t.Fatalf("delete through a link: %v", err)
	}
	// Other file types are not readable at all.
	if _, err := f.l.Read(ctx, "notes.pem"); !isInvalid(err) {
		t.Fatalf("read .pem: %v", err)
	}
	if _, err := f.l.Read(ctx, "binary.xml"); !isInvalid(err) {
		t.Fatalf("read binary: %v", err)
	}
	if _, err := f.l.Read(ctx, "sip_profiles"); !isInvalid(err) {
		t.Fatalf("read a directory: %v", err)
	}
}

func TestLocalRootIsSymlink(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.base, "conf-link")
	os.Symlink(f.root, link)
	l, err := NewLocal(link, filepath.Join(f.base, "other-backups"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.Read(ctx, "vars.xml"); err != nil || !strings.Contains(got.Content, "X-PRE-PROCESS") {
		t.Fatalf("read via a linked root: %v", err)
	}
	if _, err := l.Read(ctx, "out.xml"); err == nil {
		t.Fatal("escape via a linked root")
	}
	// Backups inside the root, or the root inside the backups, are refused.
	if _, err := NewLocal(f.root, filepath.Join(f.root, "backups")); err == nil {
		t.Fatal("backups inside the root")
	}
	if _, err := NewLocal(link, filepath.Join(f.root, "x")); err == nil {
		t.Fatal("backups inside the linked root")
	}
	if _, err := NewLocal(f.root, f.base); err == nil {
		t.Fatal("root inside the backups")
	}
	if _, err := NewLocal(filepath.Join(f.base, "missing"), filepath.Join(f.base, "b2")); err == nil {
		t.Fatal("missing root")
	}
}

func TestLocalListReadWrite(t *testing.T) {
	f := newFixture(t)
	rows, err := f.l.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range rows {
		names = append(names, fmt.Sprintf("%s:%v", e.Name, e.Dir))
	}
	// Dirs first; links that leave the root are left out.
	if got := strings.Join(names, " "); got != "autoload_configs:true linkdir:true real:true sip_profiles:true binary.xml:false inside.xml:false notes.pem:false vars.xml:false" {
		t.Fatalf("list %s", got)
	}
	if rows, err := f.l.List(ctx, "sip_profiles"); err != nil || len(rows) != 2 || rows[1].Name != "internal.xml" || rows[1].Size == 0 || rows[1].Mtime.IsZero() {
		t.Fatalf("list sip_profiles %+v %v", rows, err)
	}
	if _, err := f.l.List(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list missing: %v", err)
	}
	if _, err := f.l.List(ctx, "vars.xml"); !isInvalid(err) {
		t.Fatalf("list a file: %v", err)
	}

	vars, err := f.l.Read(ctx, "vars.xml")
	if err != nil || vars.Size != int64(len(vars.Content)) || len(vars.SHA256) != 64 || vars.Mtime.Location() != time.UTC {
		t.Fatalf("read %+v %v", vars, err)
	}

	// Stale and missing base hashes conflict and change nothing.
	var ce *ConflictError
	if _, err := f.l.Write(ctx, "vars.xml", "<include/>", "", "u"); !errors.As(err, &ce) || ce.Current != vars.SHA256 {
		t.Fatalf("write without base: %v", err)
	}
	if _, err := f.l.Write(ctx, "vars.xml", "<include/>", strings.Repeat("0", 64), "u"); !errors.As(err, &ce) || ce.Current != vars.SHA256 {
		t.Fatalf("stale base: %v", err)
	}
	if _, err := f.l.Write(ctx, "new.xml", "<include/>", vars.SHA256, "u"); !errors.As(err, &ce) || ce.Current != "" {
		t.Fatalf("base for a missing file: %v", err)
	}
	var se *SyntaxError
	if _, err := f.l.Write(ctx, "vars.xml", "<include>\n<broken>\n</include>", vars.SHA256, "u"); !errors.As(err, &se) || se.Line != 3 {
		t.Fatalf("broken XML: %v", err)
	}
	for _, bad := range []string{strings.Repeat("x", MaxSize+1), "\xff"} {
		if _, err := f.l.Write(ctx, "a.txt", bad, "", "u"); !isInvalid(err) {
			t.Fatalf("bad content: %v", err)
		}
	}
	if _, err := f.l.Write(ctx, "missing-dir/a.xml", "<a/>", "", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing directory: %v", err)
	}
	if _, err := f.l.Write(ctx, "vars.xml/x.xml", "<a/>", "", "u"); err == nil {
		t.Fatal("file as directory")
	}
	if got, _ := f.l.Read(ctx, "vars.xml"); got.SHA256 != vars.SHA256 {
		t.Fatal("a refused write changed the file")
	}

	// A write keeps the mode and backs up the old version.
	next := strings.Replace(vars.Content, "domain=", "domain_name=", 1)
	res, err := f.l.Write(ctx, "vars.xml", next, vars.SHA256, "alice")
	if err != nil || res.Path != "vars.xml" || res.Size != int64(len(next)) || res.SHA256 == vars.SHA256 {
		t.Fatalf("write %+v %v", res, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(f.root, "vars.xml")); st.Mode().Perm() != 0o640 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	if got, _ := f.l.Read(ctx, "vars.xml"); got.Content != next || got.SHA256 != res.SHA256 {
		t.Fatal("content after write")
	}
	// A new file, then .lua and other types without an XML check.
	if _, err := f.l.Write(ctx, "autoload_configs/new.conf.xml", "<configuration name=\"new.conf\"/>", "", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.l.Write(ctx, "scripts.lua", "-- <not xml", "", "alice"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(f.root, "autoload_configs"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Fatal("temp file left")
		}
	}

	hist, err := f.l.History(ctx, "vars.xml")
	if err != nil || len(hist) != 1 || hist[0].User != "alice" || hist[0].SHA256 != vars.SHA256 || hist[0].Size != vars.Size {
		t.Fatalf("history %+v %v", hist, err)
	}
	if h, _ := f.l.History(ctx, "autoload_configs/new.conf.xml"); len(h) != 0 || h == nil {
		t.Fatalf("history of a new file %+v", h)
	}
	b, err := f.l.Backup(ctx, "vars.xml", hist[0].ID)
	if err != nil || b.Content != vars.Content || b.User != "alice" || b.Path != "vars.xml" {
		t.Fatalf("backup %+v %v", b, err)
	}
	for _, id := range []string{"x", "../../etc", hist[0].ID + "0", "0000000000000000000-00000000"} {
		if _, err := f.l.Backup(ctx, "vars.xml", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("backup id %q: %v", id, err)
		}
	}
	// The backup of one file is not served for another path.
	if _, err := f.l.Backup(ctx, "sip_profiles/internal.xml", hist[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("backup under another path")
	}
	// Backups live outside the root, private.
	if within(f.l.backups, f.root) {
		t.Fatal("backups inside root")
	}
	if runtime.GOOS != "windows" {
		filepath.WalkDir(f.l.backups, func(p string, d os.DirEntry, _ error) error {
			st, _ := os.Stat(p)
			if d.IsDir() && st.Mode().Perm() != 0o700 || !d.IsDir() && st.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v", p, st.Mode().Perm())
			}
			return nil
		})
	}

	// Restore is a write: it needs the current hash and is backed up.
	if _, err := f.l.Restore(ctx, "vars.xml", hist[0].ID, vars.SHA256, "bob"); !errors.As(err, &ce) {
		t.Fatalf("restore on a stale base: %v", err)
	}
	if _, err := f.l.Restore(ctx, "vars.xml", hist[0].ID, res.SHA256, "bob"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.l.Read(ctx, "vars.xml"); got.Content != vars.Content {
		t.Fatal("restore content")
	}
	if hist, _ := f.l.History(ctx, "vars.xml"); len(hist) != 2 || hist[0].User != "bob" || hist[0].SHA256 != res.SHA256 {
		t.Fatalf("history after restore %+v", hist)
	}

	// Delete needs the current hash, is backed up and can be restored.
	cur, _ := f.l.Read(ctx, "vars.xml")
	if err := f.l.Delete(ctx, "vars.xml", "", "u"); !isInvalid(err) {
		t.Fatalf("delete without base: %v", err)
	}
	if err := f.l.Delete(ctx, "vars.xml", res.SHA256, "u"); !errors.As(err, &ce) || ce.Current != cur.SHA256 {
		t.Fatalf("delete on a stale base: %v", err)
	}
	if err := f.l.Delete(ctx, "vars.xml", cur.SHA256, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.l.Read(ctx, "vars.xml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after delete: %v", err)
	}
	if err := f.l.Delete(ctx, "vars.xml", cur.SHA256, "carol"); !errors.As(err, &ce) || ce.Current != "" {
		t.Fatalf("delete twice: %v", err)
	}
	hist, _ = f.l.History(ctx, "vars.xml")
	if _, err := f.l.Restore(ctx, "vars.xml", hist[0].ID, "", "carol"); err != nil {
		t.Fatalf("restore a deleted file: %v", err)
	}
	if got, _ := f.l.Read(ctx, "vars.xml"); got.SHA256 != cur.SHA256 {
		t.Fatal("restored content")
	}
}

func TestBackupPruning(t *testing.T) {
	f := newFixture(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	f.l.now = func() time.Time { n++; return t0.Add(time.Duration(n) * time.Second) }
	cur, _ := f.l.Read(ctx, "vars.xml")
	for i := 0; i < KeepBackups+7; i++ {
		res, err := f.l.Write(ctx, "vars.xml", fmt.Sprintf("<include><!-- %d --></include>", i), cur.SHA256, "u")
		if err != nil {
			t.Fatal(err)
		}
		cur.SHA256 = res.SHA256
	}
	hist, _ := f.l.History(ctx, "vars.xml")
	if len(hist) != KeepBackups || !hist[0].Time.Equal(t0.Add(time.Duration(KeepBackups+7)*time.Second)) || !hist[KeepBackups-1].Time.Equal(t0.Add(8*time.Second)) {
		t.Fatalf("%d backups, newest %v, oldest %v", len(hist), hist[0].Time, hist[len(hist)-1].Time)
	}
	files, _ := os.ReadDir(f.l.fileDir("vars.xml"))
	if len(files) != 2*KeepBackups {
		t.Fatalf("%d files in the backup directory", len(files))
	}
}

// TestLocalConcurrentWriters: of two writers on the same base, one wins and
// the other gets a conflict naming the winner's hash.
func TestLocalConcurrentWriters(t *testing.T) {
	f := newFixture(t)
	base, _ := f.l.Read(ctx, "vars.xml")
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		results := make([]error, 4)
		hashes := make([]string, 4)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := f.l.Write(ctx, "vars.xml", fmt.Sprintf("<include><!-- %d %d --></include>", round, i), base.SHA256, "u")
				results[i], hashes[i] = err, res.SHA256
			}()
		}
		wg.Wait()
		won := ""
		for i, err := range results {
			if err == nil {
				if won != "" {
					t.Fatalf("round %d: two writers won", round)
				}
				won = hashes[i]
			}
		}
		for _, err := range results {
			var ce *ConflictError
			if err != nil && (!errors.As(err, &ce) || ce.Current != won) {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		base, _ = f.l.Read(ctx, "vars.xml")
		if won == "" || base.SHA256 != won {
			t.Fatalf("round %d: winner %q, file %q", round, won, base.SHA256)
		}
	}
}

// TestLocalFileReplacedBetweenReadAndWrite: someone else changes the file
// on disk after the editor read it; the editor's write is refused.
func TestLocalFileReplacedBetweenReadAndWrite(t *testing.T) {
	f := newFixture(t)
	read, _ := f.l.Read(ctx, "vars.xml")
	os.WriteFile(filepath.Join(f.root, "vars.xml.tmp"), []byte("<include><!-- vi --></include>"), 0o644)
	os.Rename(filepath.Join(f.root, "vars.xml.tmp"), filepath.Join(f.root, "vars.xml"))
	var ce *ConflictError
	if _, err := f.l.Write(ctx, "vars.xml", "<include/>", read.SHA256, "u"); !errors.As(err, &ce) || ce.Current == read.SHA256 {
		t.Fatalf("write over a replaced file: %v", err)
	}
	if got, _ := f.l.Read(ctx, "vars.xml"); got.Content != "<include><!-- vi --></include>" {
		t.Fatal("the other change was overwritten")
	}
	// A file replaced by a link out of the root is refused, not followed.
	os.Remove(filepath.Join(f.root, "vars.xml"))
	os.Symlink("../secret.xml", filepath.Join(f.root, "vars.xml"))
	if _, err := f.l.Write(ctx, "vars.xml", "<x/>", "", "u"); err == nil {
		t.Fatal("wrote through a planted link")
	}
	if data, _ := os.ReadFile(filepath.Join(f.base, "secret.xml")); string(data) != "<secret/>" {
		t.Fatal("secret changed")
	}
}
