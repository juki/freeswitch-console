package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/juki/freeswitch-console/internal/conffs"
)

// Files is the sample configuration as a conffs.FileStore: it can be listed,
// read and searched; writing, deleting and restoring fail with ErrDisabled,
// and no file has a history.
type Files struct{}

var _ conffs.FileStore = Files{}

// ErrDisabled is the answer to a change of the sample configuration; the
// server answers it with 403.
var ErrDisabled error = kindError{conffs.ErrForbidden, "disabled in the demo"}

// confTime is the modification time of every sample file.
var confTime = time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)

// kindError is an error of a conffs kind that shows only its message.
type kindError struct {
	kind error
	msg  string
}

func (e kindError) Error() string        { return e.msg }
func (e kindError) Is(target error) bool { return target == e.kind }

func (Files) List(_ context.Context, dir string) ([]conffs.Entry, error) {
	d, err := conffs.CleanPath(dir, false)
	if err != nil {
		return nil, err
	}
	items, err := fs.ReadDir(confRoot, d)
	if err != nil {
		if st, serr := fs.Stat(confRoot, d); serr == nil && !st.IsDir() {
			return nil, kindError{conffs.ErrInvalid, "not a directory"}
		}
		return nil, kindError{conffs.ErrNotFound, "directory not found"}
	}
	out := []conffs.Entry{}
	for _, it := range items {
		e := conffs.Entry{Name: it.Name(), Dir: it.IsDir(), Mtime: confTime}
		if info, err := it.Info(); err == nil && !e.Dir {
			e.Size = info.Size()
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b conffs.Entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (Files) Read(_ context.Context, p string) (conffs.File, error) {
	c, err := conffs.CleanPath(p, true)
	if err != nil {
		return conffs.File{}, err
	}
	data, err := readConf(c)
	if err != nil {
		return conffs.File{}, err
	}
	h := sha256.Sum256(data)
	return conffs.File{Path: c, Content: string(data), SHA256: hex.EncodeToString(h[:]), Size: int64(len(data)), Mtime: confTime}, nil
}

func readConf(c string) ([]byte, error) {
	if st, err := fs.Stat(confRoot, c); err != nil {
		return nil, kindError{conffs.ErrNotFound, "file not found"}
	} else if st.IsDir() {
		return nil, kindError{conffs.ErrInvalid, c + " is not a regular file"}
	}
	return fs.ReadFile(confRoot, c)
}

func (Files) Write(context.Context, string, string, string, string) (conffs.Written, error) {
	return conffs.Written{}, ErrDisabled
}

func (Files) Delete(context.Context, string, string, string) error {
	return ErrDisabled
}

func (Files) Restore(context.Context, string, string, string, string) (conffs.Written, error) {
	return conffs.Written{}, ErrDisabled
}

func (Files) History(_ context.Context, p string) ([]conffs.Backup, error) {
	if _, err := conffs.CleanPath(p, true); err != nil {
		return nil, err
	}
	return []conffs.Backup{}, nil
}

func (Files) Backup(context.Context, string, string) (conffs.BackupContent, error) {
	return conffs.BackupContent{}, kindError{conffs.ErrNotFound, "backup not found"}
}

// Search finds query in every file, as conffs.Local does: by path, then
// line, at most opts.Limit matches.
func (Files) Search(_ context.Context, query string, opts conffs.SearchOptions) (conffs.SearchResult, error) {
	q, _, err := conffs.ParseSearch(url.Values{"q": {query}})
	if err != nil {
		return conffs.SearchResult{}, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = conffs.DefaultSearchLimit
	}
	limit = min(limit, conffs.MaxSearchLimit)
	needle := q
	if !opts.CaseSensitive {
		needle = strings.ToLower(q)
	}
	var files []string
	fs.WalkDir(confRoot, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	slices.Sort(files)
	res := conffs.SearchResult{Rows: []conffs.Match{}}
	for _, p := range files {
		data, err := fs.ReadFile(confRoot, p)
		if err != nil {
			return conffs.SearchResult{}, err
		}
		for i, line := range strings.Split(string(data), "\n") {
			hay := line
			if !opts.CaseSensitive {
				hay = strings.ToLower(line)
			}
			if !strings.Contains(hay, needle) {
				continue
			}
			if len(res.Rows) == limit {
				res.Truncated = true
				return res, nil
			}
			res.Rows = append(res.Rows, conffs.Match{Path: p, Line: i + 1, Text: clip(strings.TrimSpace(line))})
		}
	}
	return res, nil
}

// clip cuts a matching line to 300 characters, as conffs does; the sample
// lines are all shorter.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= 300 {
		return s
	}
	return string([]rune(s)[:299]) + "…"
}
