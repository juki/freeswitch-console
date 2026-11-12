package web

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	entryRe       = regexp.MustCompile(`^\s*'([^']+)':\s*(.+),\s*$`)
	placeholderRe = regexp.MustCompile(`\{(\w+)\}`)
	useRe         = regexp.MustCompile(`\btx?\(\s*'([^']+)'`)
	dynamicUseRe  = regexp.MustCompile(`\btx?\(\s*[^'\s)]`)
)

// catalog reads a locale module: one 'key': 'text', entry per line.
func catalog(t *testing.T, name string) map[string]string {
	t.Helper()
	data, err := fs.ReadFile(files, "static/js/locales/"+name)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		m := entryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, dup := out[m[1]]; dup {
			t.Errorf("%s: duplicate key %q", name, m[1])
		}
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("%s: no entries found", name)
	}
	return out
}

func placeholders(s string) string {
	var names []string
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// TestCatalogsMatch keeps the languages complete: the same keys and the same
// placeholders in every catalog.
func TestCatalogsMatch(t *testing.T) {
	en := catalog(t, "en.js")
	zh := catalog(t, "zh-CN.js")
	for k, v := range en {
		other, ok := zh[k]
		if !ok {
			t.Errorf("zh-CN.js lacks %q", k)
			continue
		}
		if a, b := placeholders(v), placeholders(other); a != b {
			t.Errorf("%q: placeholders differ: en {%s}, zh-CN {%s}", k, a, b)
		}
	}
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Errorf("en.js lacks %q", k)
		}
	}
}

// TestKeysUsedExist finds every t('key') and tx('key') call in the frontend:
// the key must be a literal and must exist, and no catalog entry may be left
// unused.
func TestKeysUsedExist(t *testing.T) {
	en := catalog(t, "en.js")
	used := map[string]bool{}
	err := fs.WalkDir(files, "static/js", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") ||
			strings.Contains(path, "/locales/") || strings.HasSuffix(path, "/i18n.js") {
			return err
		}
		data, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range useRe.FindAllStringSubmatch(line, -1) {
				used[m[1]] = true
				if _, ok := en[m[1]]; !ok {
					t.Errorf("%s:%d uses %q, which is in no catalog", path, i+1, m[1])
				}
			}
			if dynamicUseRe.MatchString(line) {
				t.Errorf("%s:%d calls t() without a literal key: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for k := range en {
		if !used[k] {
			t.Errorf("catalog key %q is not used", k)
		}
	}
}
