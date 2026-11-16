package conffs

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func rowsOf(res SearchResult) []string {
	var out []string
	for _, m := range res.Rows {
		out = append(out, fmt.Sprintf("%s:%d:%s", m.Path, m.Line, m.Text))
	}
	return out
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	put := func(p, s string) { os.WriteFile(filepath.Join(f.root, p), []byte(s), 0o644) }
	put("sip_profiles/external.xml", "<profile name=\"external\">\n  <param name=\"Secret-Thing\" value=\"x\"/>\n\t<!-- secret -->  \r\n</profile>\n")
	put("a.txt", "SECRET here\nnothing\n")
	put(".fsconsole-tmp-0123", "<secret/>")
	put("big.xml", "<a>secret"+strings.Repeat("x", MaxSize)+"</a>")
	put("latin1.xml", "<a>secret \xe9</a>")
	put("notes.pem", "secret")
	os.Symlink(".", filepath.Join(f.root, "loop")) // a link back to the root
	os.WriteFile(filepath.Join(f.root, "real", "target.xml"), []byte("<include><!-- secret in target --></include>"), 0o644)

	res, err := f.l.Search(ctx, "secret", SearchOptions{})
	want := []string{
		"a.txt:1:SECRET here",
		"inside.xml:1:<include><!-- secret in target --></include>", // a linked file inside the root
		"real/target.xml:1:<include><!-- secret in target --></include>",
		`sip_profiles/external.xml:2:<param name="Secret-Thing" value="x"/>`,
		"sip_profiles/external.xml:3:<!-- secret -->",
	}
	// Not: out.xml/outabs.xml/updir (outside the root), loop and linkdir
	// (linked directories are not entered), the temp file, big.xml, the
	// Latin-1 file, the .pem file.
	if err != nil || res.Truncated || !reflect.DeepEqual(rowsOf(res), want) {
		t.Fatalf("search:\n%s\n%v truncated=%v", strings.Join(rowsOf(res), "\n"), err, res.Truncated)
	}

	res, _ = f.l.Search(ctx, "Secret", SearchOptions{CaseSensitive: true})
	if got := rowsOf(res); len(got) != 1 || !strings.Contains(got[0], "Secret-Thing") {
		t.Fatalf("case-sensitive %q", got)
	}
	// A literal, not a pattern.
	if res, _ := f.l.Search(ctx, "$${local_ip_v4}", SearchOptions{}); len(res.Rows) != 1 || res.Rows[0].Path != "vars.xml" {
		t.Fatalf("literal %q", rowsOf(res))
	}
	if res, _ := f.l.Search(ctx, "s.c.e", SearchOptions{}); len(res.Rows) != 0 {
		t.Fatal("pattern matched")
	}

	// Limit: more matches than asked.
	res, _ = f.l.Search(ctx, "secret", SearchOptions{Limit: 2})
	if len(res.Rows) != 2 || !res.Truncated || res.Rows[1].Path != "inside.xml" {
		t.Fatalf("limit %q %v", rowsOf(res), res.Truncated)
	}
	res, _ = f.l.Search(ctx, "secret", SearchOptions{Limit: 5})
	if len(res.Rows) != 5 || res.Truncated {
		t.Fatalf("exact limit %v", res.Truncated)
	}
	// File bound and time bound.
	f.l.searchFiles = 2
	if res, _ := f.l.Search(ctx, "secret", SearchOptions{}); !res.Truncated {
		t.Fatal("file bound")
	}
	f.l.searchFiles, f.l.searchTime = maxSearchFiles, 0
	if res, _ := f.l.Search(ctx, "secret", SearchOptions{}); !res.Truncated || len(res.Rows) != 0 {
		t.Fatalf("time bound %v", rowsOf(res))
	}
}

func TestSearchHugeDirectory(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "many")
	os.Mkdir(dir, 0o755)
	for i := 0; i < 1200; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.xml", i)), []byte("<x>needle</x>"), 0o644)
	}
	f.l.searchFiles = 1000
	res, err := f.l.Search(ctx, "needle", SearchOptions{Limit: MaxSearchLimit})
	if err != nil || !res.Truncated || len(res.Rows) != MaxSearchLimit {
		t.Fatalf("%d rows truncated=%v %v", len(res.Rows), res.Truncated, err)
	}
	// Sorted by path.
	for i := 1; i < len(res.Rows); i++ {
		if res.Rows[i-1].Path >= res.Rows[i].Path {
			t.Fatalf("order %s %s", res.Rows[i-1].Path, res.Rows[i].Path)
		}
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt("   short line\t", 3); got != "short line" {
		t.Fatalf("%q", got)
	}
	// A long line of multi-byte characters with the match far in: 300
	// characters around it, cut at characters, marked at both ends.
	line := strings.Repeat("é", 1000) + "NEEDLE" + strings.Repeat("ü", 1000)
	got := excerpt(line, 1000)
	if utf8.RuneCountInString(got) != maxMatchText || !utf8.ValidString(got) || !strings.Contains(got, "NEEDLE") || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("%d runes %q", utf8.RuneCountInString(got), got)
	}
	start := excerpt("NEEDLE"+strings.Repeat("ü", 500), 0)
	if !strings.HasPrefix(start, "NEEDLE") || !strings.HasSuffix(start, "…") || utf8.RuneCountInString(start) != maxMatchText {
		t.Fatalf("start %q", start)
	}
	end := excerpt(strings.Repeat("ü", 500)+"NEEDLE", 500)
	if !strings.HasSuffix(end, "NEEDLE") || !strings.HasPrefix(end, "…") {
		t.Fatalf("end %q", end)
	}

	// Through Search, with a case-insensitive match after characters whose
	// lower case differs.
	f := newFixture(t)
	os.WriteFile(filepath.Join(f.root, "long.txt"), []byte(strings.Repeat("ÄÖ", 400)+" Needle "+strings.Repeat("x", 400)), 0o644)
	res, _ := f.l.Search(ctx, "needle", SearchOptions{})
	if len(res.Rows) != 1 || !strings.Contains(res.Rows[0].Text, "Needle") || utf8.RuneCountInString(res.Rows[0].Text) != maxMatchText {
		t.Fatalf("%q", rowsOf(res))
	}
}

func TestParseSearch(t *testing.T) {
	for raw, want := range map[string]SearchOptions{
		"q=ab":                          {Limit: 100},
		"q=ab&case=0&limit=7":           {Limit: 7},
		"q=ab&case=1":                   {CaseSensitive: true, Limit: 100},
		"q=ab&limit=100000":             {Limit: MaxSearchLimit},
		"q=" + strings.Repeat("é", 200): {Limit: 100},
	} {
		v, _ := url.ParseQuery(raw)
		if _, got, err := ParseSearch(v); err != nil || got != want {
			t.Errorf("%s: %+v %v", raw[:min(len(raw), 30)], got, err)
		}
	}
	for _, raw := range []string{"", "q=a", "q=" + strings.Repeat("x", 201), "q=a%0Ab", "q=a%0Db", "q=ab&case=2", "q=ab&case=true", "q=ab&limit=0", "q=ab&limit=-1", "q=ab&limit=x"} {
		v, _ := url.ParseQuery(raw)
		if _, _, err := ParseSearch(v); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted", raw)
		}
	}
	f := newFixture(t)
	if _, err := f.l.Search(ctx, "a", SearchOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("one character")
	}
}

func TestSearchOverAgent(t *testing.T) {
	f, c, _ := newAgent(t)
	local, _ := f.l.Search(ctx, "include", SearchOptions{})
	remote, err := c.Search(ctx, "include", SearchOptions{})
	if err != nil || !reflect.DeepEqual(local, remote) || len(remote.Rows) == 0 {
		t.Fatalf("agent %v\nlocal  %q\nremote %q", err, rowsOf(local), rowsOf(remote))
	}
	if r, _ := c.Search(ctx, "include", SearchOptions{Limit: 1, CaseSensitive: true}); len(r.Rows) != 1 || !r.Truncated {
		t.Fatalf("options over the agent %+v", r)
	}
	if _, err := c.Search(ctx, "x", SearchOptions{}); !errors.Is(err, ErrInvalid) || err.Error() != "q must be 2 to 200 characters on one line" {
		t.Fatalf("invalid over the agent: %v", err)
	}
}
