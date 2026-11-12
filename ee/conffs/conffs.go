//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

// Package conffs reads and writes the FreeSWITCH configuration files below
// one root directory, on this host (Local) or through the fsdeck agent
// (Client), with optimistic concurrency and a backup of every overwritten
// or deleted version.
package conffs

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxSize is the largest file that can be read or written.
	MaxSize = 1 << 20
	// KeepBackups is how many backups are kept per file.
	KeepBackups = 50
)

// Extensions are the file types that can be read and written.
var Extensions = []string{".xml", ".lua", ".conf", ".tpl", ".txt", ".json", ".cfg", ".ini"}

// Entry is one item of a directory listing.
type Entry struct {
	Name  string    `json:"name"`
	Dir   bool      `json:"dir"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
}

// File is a file with its content.
type File struct {
	Path    string    `json:"path"`
	Content string    `json:"content"`
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	Mtime   time.Time `json:"mtime"`
}

// Written describes a file after a write or a restore.
type Written struct {
	Path   string    `json:"path"`
	SHA256 string    `json:"sha256"`
	Size   int64     `json:"size"`
	Mtime  time.Time `json:"mtime"`
}

// Backup describes one saved version of a file.
type Backup struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	User   string    `json:"user"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
}

// BackupContent is a saved version with its content.
type BackupContent struct {
	Path    string    `json:"path"`
	ID      string    `json:"id"`
	Content string    `json:"content"`
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	Time    time.Time `json:"time"`
	User    string    `json:"user"`
}

// FileStore is the configuration directory of one FreeSWITCH. Paths are
// relative to its root with "/" separators. A baseSHA256 is the hash of the
// file the change is based on ("" for a file that must not exist yet); a
// mismatch is a *ConflictError. user is recorded with the backup.
type FileStore interface {
	List(ctx context.Context, dir string) ([]Entry, error)
	Read(ctx context.Context, path string) (File, error)
	Write(ctx context.Context, path, content, baseSHA256, user string) (Written, error)
	Delete(ctx context.Context, path, baseSHA256, user string) error
	History(ctx context.Context, path string) ([]Backup, error)
	Backup(ctx context.Context, path, id string) (BackupContent, error)
	Restore(ctx context.Context, path, id, baseSHA256, user string) (Written, error)
	Search(ctx context.Context, query string, opts SearchOptions) (SearchResult, error)
}

// Search bounds.
const (
	DefaultSearchLimit = 100
	MaxSearchLimit     = 500
	maxSearchFiles     = 5000
	searchTime         = 5 * time.Second
	maxMatchText       = 300 // characters of a matching line
)

// SearchOptions shape a search; a Limit of 0 is DefaultSearchLimit.
type SearchOptions struct {
	CaseSensitive bool
	Limit         int
}

// Match is one line containing the query.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"` // 1-based
	Text string `json:"text"` // trimmed, at most 300 characters
}

// SearchResult holds the matches by path, then line. Truncated is set when
// there are more matches than the limit or a bound on files or time cut the
// search short.
type SearchResult struct {
	Rows      []Match `json:"rows"`
	Truncated bool    `json:"truncated"`
}

// ParseSearch reads q, case and limit of a search request: q is a literal
// of 2 to 200 characters on one line, case 0 or 1, limit at least 1 and
// capped at MaxSearchLimit.
func ParseSearch(v url.Values) (string, SearchOptions, error) {
	q := v.Get("q")
	if n := utf8.RuneCountInString(q); n < 2 || n > 200 || strings.ContainsAny(q, "\r\n") {
		return "", SearchOptions{}, invalid("q must be 2 to 200 characters on one line")
	}
	var opts SearchOptions
	switch v.Get("case") {
	case "", "0":
	case "1":
		opts.CaseSensitive = true
	default:
		return "", SearchOptions{}, invalid("case must be 0 or 1")
	}
	opts.Limit = DefaultSearchLimit
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			return "", SearchOptions{}, invalid("limit must be a positive number")
		}
		opts.Limit = min(n, MaxSearchLimit)
	}
	return q, opts, nil
}

func (o SearchOptions) values(q string) url.Values {
	v := url.Values{"q": {q}, "case": {"0"}, "limit": {strconv.Itoa(o.Limit)}}
	if o.CaseSensitive {
		v.Set("case", "1")
	}
	return v
}

// excerpt is line trimmed and, when longer than maxMatchText characters,
// cut to a window around the match at rune index at, marked with "…".
func excerpt(line string, at int) string {
	r := []rune(line)
	start, end := 0, len(r)
	for start < end && unicode.IsSpace(r[start]) {
		start++
	}
	for end > start && unicode.IsSpace(r[end-1]) {
		end--
	}
	r, at = r[start:end], max(0, at-start)
	if len(r) <= maxMatchText {
		return string(r)
	}
	from := max(0, at-maxMatchText/3)
	to := min(len(r), from+maxMatchText)
	from = to - maxMatchText
	out := append([]rune(nil), r[from:to]...)
	if from > 0 {
		out[0] = '…'
	}
	if to < len(r) {
		out[len(out)-1] = '…'
	}
	return string(out)
}

var (
	// ErrInvalid marks a request that cannot be carried out as asked: a bad
	// path, a file type that is not editable, a file too large.
	ErrInvalid = errors.New("invalid request")
	// ErrNotFound marks a missing file, directory or backup.
	ErrNotFound = errors.New("not found")
	// ErrAgent marks a failure to reach the agent or an answer it should
	// not have given, such as a rejected token.
	ErrAgent = errors.New("config agent")
)

// kindError is an error message of a kind (ErrInvalid or ErrNotFound) that
// shows only the message.
type kindError struct {
	kind error
	msg  string
}

func (e *kindError) Error() string        { return e.msg }
func (e *kindError) Is(target error) bool { return target == e.kind }

func invalid(format string, a ...any) error {
	return &kindError{ErrInvalid, fmt.Sprintf(format, a...)}
}

func notFound(what string) error { return &kindError{ErrNotFound, what + " not found"} }

// ConflictError is a change based on another version than the current one.
// Current is the hash of the current file, "" when there is none.
type ConflictError struct{ Current string }

func (e *ConflictError) Error() string {
	if e.Current == "" {
		return "the file does not exist (any more)"
	}
	return "the file has changed since it was read"
}

// SyntaxError is a .xml file that is not well-formed.
type SyntaxError struct {
	Line int
	Msg  string
}

const syntaxPrefix = "the XML is not well-formed: "

func (e *SyntaxError) Error() string { return syntaxPrefix + e.Msg }

// CleanPath checks a client path and returns it cleaned. "" and "." name
// the root, which only a directory operation accepts (file false).
func CleanPath(p string, file bool) (string, error) {
	if len(p) > 1024 {
		return "", invalid("path is too long")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", invalid("path contains a forbidden character")
		}
	}
	if strings.HasPrefix(p, "/") {
		return "", invalid("path must be relative to the configuration directory")
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return "", invalid("path must not contain ..")
	}
	c := path.Clean("./" + p)
	if file && c == "." {
		return "", invalid("path names no file")
	}
	if file && !slices.Contains(Extensions, strings.ToLower(path.Ext(c))) {
		return "", invalid("only %s files can be edited", strings.Join(Extensions, " "))
	}
	return c, nil
}

// CheckXML reports whether content is well-formed XML. FreeSWITCH
// specifics (X-PRE-PROCESS, $${var}, an <include> root) are ordinary XML;
// whitespace-only content passes, as FreeSWITCH reads an empty include.
func CheckXML(content string) error {
	d := xml.NewDecoder(strings.NewReader(content))
	d.Strict = true
	// Only the structure matters here, so a declared encoding is taken as is.
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	for {
		_, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			var se *xml.SyntaxError
			if errors.As(err, &se) {
				return &SyntaxError{Line: se.Line, Msg: se.Msg}
			}
			line, _ := d.InputPos()
			return &SyntaxError{Line: line, Msg: err.Error()}
		}
	}
}

// errorBody is the JSON of an error answer, shared by fsdeck and the agent.
type errorBody struct {
	Error         string  `json:"error"`
	CurrentSHA256 *string `json:"currentSha256,omitempty"`
	Line          *int    `json:"line,omitempty"`
}

// WriteError answers err with its status: 400 invalid, 404 not found,
// 409 {"error","currentSha256"} conflict, 422 {"error","line"} not
// well-formed, 502 agent trouble, 500 anything else.
func WriteError(w http.ResponseWriter, err error) {
	body := errorBody{Error: err.Error()}
	code := http.StatusInternalServerError
	var ce *ConflictError
	var se *SyntaxError
	switch {
	case errors.As(err, &ce):
		code, body.CurrentSHA256 = http.StatusConflict, &ce.Current
	case errors.As(err, &se):
		code, body.Line = http.StatusUnprocessableEntity, &se.Line
	case errors.Is(err, ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrAgent):
		code = http.StatusBadGateway
	}
	writeJSON(w, code, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
