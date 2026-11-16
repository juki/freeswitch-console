package conffs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// tmpPrefix starts the name of a file being written; listings hide it.
const tmpPrefix = ".fsconsole-tmp-"

var backupIDRE = regexp.MustCompile(`^[0-9]{19}-[0-9a-f]{8}$`)

// Local is a FileStore on this host. Every access goes through os.Root, so
// neither a path nor a symbolic link can reach outside the root; a symbolic
// link inside the root that points elsewhere inside it is followed when
// reading, but a write or delete never goes through a link.
type Local struct {
	root    string
	backups string // outside root: one directory per file

	// mu makes the hash check and the replacement one step for writers
	// in this process.
	mu  sync.Mutex
	now func() time.Time

	// Search bounds, fields so tests can lower them.
	searchFiles int
	searchTime  time.Duration
}

// NewLocal serves the directory root and keeps backups under backups,
// which is created (mode 0700) and must not lie inside root.
func NewLocal(root, backups string) (*Local, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("configuration directory: %w", err)
	}
	if st, err := os.Stat(rootReal); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("configuration directory %s is not a directory", root)
	}
	if err := os.MkdirAll(backups, 0o700); err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	backupsReal, err := filepath.EvalSymlinks(backups)
	if err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	if within(backupsReal, rootReal) || within(rootReal, backupsReal) {
		return nil, errors.New("the backup directory must not overlap the configuration directory")
	}
	return &Local{root: root, backups: backupsReal, now: time.Now, searchFiles: maxSearchFiles, searchTime: searchTime}, nil
}

// within reports whether p is dir or below it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (l *Local) open() (*os.Root, error) {
	r, err := os.OpenRoot(l.root)
	if err != nil {
		return nil, fmt.Errorf("configuration directory: %w", err)
	}
	return r, nil
}

// rootError turns an os.Root error for p into an answer for the client.
func rootError(err error, what string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notFound(what)
	case strings.Contains(err.Error(), "escapes"):
		return invalid("path leads outside the configuration directory")
	case errors.Is(err, fs.ErrPermission):
		return invalid("permission denied")
	}
	return err
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (l *Local) List(_ context.Context, dir string) ([]Entry, error) {
	d, err := CleanPath(dir, false)
	if err != nil {
		return nil, err
	}
	r, err := l.open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(d)
	if err != nil {
		return nil, rootError(err, "directory")
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.IsDir() {
		return nil, invalid("not a directory")
	}
	items, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, it := range items {
		if strings.HasPrefix(it.Name(), tmpPrefix) {
			continue
		}
		// Stat through the root: a link that leaves it, or a broken one, is
		// left out.
		st, err := r.Stat(path.Join(d, it.Name()))
		if err != nil {
			continue
		}
		e := Entry{Name: it.Name(), Dir: st.IsDir(), Mtime: st.ModTime().UTC()}
		if !e.Dir {
			e.Size = st.Size()
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int {
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

func (l *Local) Read(_ context.Context, p string) (File, error) {
	c, err := CleanPath(p, true)
	if err != nil {
		return File{}, err
	}
	r, err := l.open()
	if err != nil {
		return File{}, err
	}
	defer r.Close()
	data, st, err := readFile(r, c, true)
	if err != nil {
		return File{}, err
	}
	return File{Path: c, Content: string(data), SHA256: sum(data), Size: st.Size(), Mtime: st.ModTime().UTC()}, nil
}

// readFile reads a regular text file of at most MaxSize. follow allows a
// symbolic link (inside the root) as the last element.
func readFile(r *os.Root, c string, follow bool) ([]byte, fs.FileInfo, error) {
	if !follow {
		lst, err := r.Lstat(c)
		if err != nil {
			return nil, nil, rootError(err, "file")
		}
		if lst.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, invalid("%s is a symbolic link; edit the file it points to", c)
		}
	}
	// Opening a FIFO blocks until a writer appears, so the type is checked
	// before the open as well as after it.
	if st, err := r.Stat(c); err != nil {
		return nil, nil, rootError(err, "file")
	} else if !st.Mode().IsRegular() {
		return nil, nil, invalid("%s is not a regular file", c)
	}
	f, err := r.Open(c)
	if err != nil {
		return nil, nil, rootError(err, "file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, invalid("%s is not a regular file", c)
	}
	if st.Size() > MaxSize {
		return nil, nil, invalid("%s is larger than 1 MiB", c)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxSize {
		return nil, nil, invalid("%s is larger than 1 MiB", c)
	}
	if !utf8.Valid(data) {
		return nil, nil, invalid("%s is not UTF-8 text", c)
	}
	return data, st, nil
}

// current returns the file at c, or exists false when there is none.
func current(r *os.Root, c string) (data []byte, st fs.FileInfo, exists bool, err error) {
	data, st, err = readFile(r, c, false)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, false, nil
	}
	return data, st, err == nil, err
}

func (l *Local) Write(_ context.Context, p, content, baseSHA256, user string) (Written, error) {
	c, err := CleanPath(p, true)
	if err != nil {
		return Written{}, err
	}
	if len(content) > MaxSize {
		return Written{}, invalid("content is larger than 1 MiB")
	}
	if !utf8.ValidString(content) {
		return Written{}, invalid("content is not UTF-8 text")
	}
	if strings.EqualFold(path.Ext(c), ".xml") {
		if err := CheckXML(content); err != nil {
			return Written{}, err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.open()
	if err != nil {
		return Written{}, err
	}
	defer r.Close()
	return l.write(r, c, []byte(content), baseSHA256, user)
}

// write replaces c atomically after checking baseSHA256. l.mu is held.
func (l *Local) write(r *os.Root, c string, content []byte, baseSHA256, user string) (Written, error) {
	dir := path.Dir(c)
	if st, err := r.Stat(dir); err != nil {
		return Written{}, rootError(err, "directory")
	} else if !st.IsDir() {
		return Written{}, invalid("%s is not a directory", dir)
	}
	old, oldSt, exists, err := current(r, c)
	if err != nil {
		return Written{}, err
	}
	cur := ""
	if exists {
		cur = sum(old)
	}
	if cur != baseSHA256 {
		return Written{}, &ConflictError{Current: cur}
	}
	mode := fs.FileMode(0o644)
	if exists {
		mode = oldSt.Mode().Perm()
		if err := l.backup(c, old, user); err != nil {
			return Written{}, err
		}
	}

	var rnd [8]byte
	rand.Read(rnd[:])
	tmp := path.Join(dir, tmpPrefix+hex.EncodeToString(rnd[:]))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Written{}, rootError(err, "directory")
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Chmod(mode) // OpenFile's mode is cut by the umask
	}
	if err == nil && exists {
		keepOwner(f, oldSt)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.Rename(tmp, c)
	}
	if err != nil {
		r.Remove(tmp)
		return Written{}, err
	}
	st, err := r.Stat(c)
	if err != nil {
		return Written{}, err
	}
	return Written{Path: c, SHA256: sum(content), Size: st.Size(), Mtime: st.ModTime().UTC()}, nil
}

func (l *Local) Delete(_ context.Context, p, baseSHA256, user string) error {
	c, err := CleanPath(p, true)
	if err != nil {
		return err
	}
	if baseSHA256 == "" {
		return invalid("baseSha256 is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.open()
	if err != nil {
		return err
	}
	defer r.Close()
	old, _, exists, err := current(r, c)
	if err != nil {
		return err
	}
	if cur := sum(old); !exists || cur != baseSHA256 {
		if !exists {
			cur = ""
		}
		return &ConflictError{Current: cur}
	}
	if err := l.backup(c, old, user); err != nil {
		return err
	}
	return r.Remove(c)
}

// backupMeta is the stored description of a backup; the content lies
// beside it in <id>.data.
type backupMeta struct {
	Backup
	Path string `json:"path"`
}

// fileDir is the backup directory of the file at c: a hash, so no client
// text becomes part of a host path.
func (l *Local) fileDir(c string) string {
	return filepath.Join(l.backups, sum([]byte(c))[:32])
}

// backup saves content as the newest version of c and keeps the newest
// KeepBackups. l.mu is held.
func (l *Local) backup(c string, content []byte, user string) error {
	dir := l.fileDir(c)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	now := l.now().UTC()
	var rnd [4]byte
	rand.Read(rnd[:])
	id := fmt.Sprintf("%019d-%08x", now.UnixNano(), binary.BigEndian.Uint32(rnd[:]))
	meta, err := json.Marshal(backupMeta{Backup: Backup{ID: id, Time: now, User: user, Size: int64(len(content)), SHA256: sum(content)}, Path: c})
	if err != nil {
		return err
	}
	// The data first: a description without its content is never seen.
	if err := os.WriteFile(filepath.Join(dir, id+".data"), content, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), meta, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	ids, err := backupIDs(dir)
	if err != nil {
		return err
	}
	for _, old := range ids[min(len(ids), KeepBackups):] {
		os.Remove(filepath.Join(dir, old+".json"))
		os.Remove(filepath.Join(dir, old+".data"))
	}
	return nil
}

// backupIDs lists the backups in dir, newest first.
func backupIDs(dir string) ([]string, error) {
	items, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, it := range items {
		if id, ok := strings.CutSuffix(it.Name(), ".json"); ok && backupIDRE.MatchString(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	slices.Reverse(ids)
	return ids, nil
}

func readMeta(dir, id string) (backupMeta, error) {
	var m backupMeta
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	return m, err
}

func (l *Local) History(_ context.Context, p string) ([]Backup, error) {
	c, err := CleanPath(p, true)
	if err != nil {
		return nil, err
	}
	dir := l.fileDir(c)
	ids, err := backupIDs(dir)
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, id := range ids {
		if m, err := readMeta(dir, id); err == nil && m.Path == c {
			out = append(out, m.Backup)
		}
	}
	return out, nil
}

func (l *Local) Backup(_ context.Context, p, id string) (BackupContent, error) {
	c, err := CleanPath(p, true)
	if err != nil {
		return BackupContent{}, err
	}
	if !backupIDRE.MatchString(id) {
		return BackupContent{}, notFound("backup")
	}
	dir := l.fileDir(c)
	m, err := readMeta(dir, id)
	if err != nil || m.Path != c {
		return BackupContent{}, notFound("backup")
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".data"))
	if err != nil {
		return BackupContent{}, notFound("backup")
	}
	return BackupContent{Path: c, ID: id, Content: string(data), SHA256: sum(data), Size: int64(len(data)), Time: m.Time, User: m.User}, nil
}

func (l *Local) Restore(ctx context.Context, p, id, baseSHA256, user string) (Written, error) {
	b, err := l.Backup(ctx, p, id)
	if err != nil {
		return Written{}, err
	}
	return l.Write(ctx, b.Path, b.Content, baseSHA256, user)
}

// Search looks for query in every file Read would open: the allowed types,
// at most 1 MiB, UTF-8. Others are skipped. The walk stays inside the root,
// does not enter linked directories (a link may loop) and treats a linked
// file as Read does.
func (l *Local) Search(ctx context.Context, query string, opts SearchOptions) (SearchResult, error) {
	if opts.Limit == 0 {
		opts.Limit = DefaultSearchLimit
	}
	q, o, err := ParseSearch(opts.values(query))
	if err != nil {
		return SearchResult{}, err
	}
	r, err := l.open()
	if err != nil {
		return SearchResult{}, err
	}
	defer r.Close()
	deadline := time.Now().Add(l.searchTime)
	expired := func() bool { return ctx.Err() != nil || time.Now().After(deadline) }

	files, cut := l.searchable(r, expired)
	res := SearchResult{Rows: []Match{}, Truncated: cut}
	needle := q
	if !o.CaseSensitive {
		needle = strings.ToLower(q)
	}
	for _, p := range files {
		if expired() {
			res.Truncated = true
			break
		}
		// One read: a file that changes meanwhile is searched as read.
		data, _, err := readFile(r, p, true)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			hay := line
			if !o.CaseSensitive {
				// ToLower maps rune by rune, so rune positions still match.
				hay = strings.ToLower(line)
			}
			at := strings.Index(hay, needle)
			if at < 0 {
				continue
			}
			if len(res.Rows) == o.Limit {
				res.Truncated = true
				return res, nil
			}
			res.Rows = append(res.Rows, Match{Path: p, Line: i + 1, Text: excerpt(line, utf8.RuneCountInString(hay[:at]))})
		}
	}
	return res, nil
}

// searchable lists the files a search reads, sorted by path, at most
// l.searchFiles of them; cut is true when it stopped early.
func (l *Local) searchable(r *os.Root, expired func() bool) (files []string, cut bool) {
	defer func() { slices.Sort(files) }()
	dirs := []string{"."}
	for len(dirs) > 0 {
		d := dirs[0]
		dirs = dirs[1:]
		f, err := r.Open(d)
		if err != nil {
			continue
		}
		for {
			// Batches keep a huge directory from holding the walk past
			// its time.
			items, err := f.ReadDir(512)
			for _, it := range items {
				name := it.Name()
				if strings.HasPrefix(name, tmpPrefix) {
					continue
				}
				p := path.Join(d, name)
				switch {
				case it.IsDir():
					dirs = append(dirs, p)
				case it.Type().IsRegular() || it.Type()&fs.ModeSymlink != 0:
					if _, err := CleanPath(p, true); err != nil {
						continue
					}
					if len(files) == l.searchFiles {
						f.Close()
						return files, true
					}
					files = append(files, p)
				}
			}
			if err != nil || len(items) == 0 {
				break
			}
			if expired() {
				f.Close()
				return files, true
			}
		}
		f.Close()
	}
	return files, false
}
