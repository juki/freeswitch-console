// Package audit appends one JSON line per security-relevant action to a file
// and reads them back for the console.
package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Results of an audited action.
const (
	OK     = "ok"
	Denied = "denied"
	Error  = "error"
)

// Entry is one line of the log. Every field is always written.
type Entry struct {
	Time   string `json:"time"` // RFC 3339, UTC
	User   string `json:"user"`
	IP     string `json:"ip"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	Result string `json:"result"`
	Error  string `json:"error"`
}

// Log is an append-only JSON Lines file. A nil *Log is a disabled log: Write
// does nothing and Read returns no rows. Safe for concurrent use.
type Log struct {
	path    string
	maxSize int64 // the file is rotated to path+".1" before it would exceed this

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens or creates the log at path (mode 0600).
func Open(path string) (*Log, error) {
	l := &Log{path: path, maxSize: 10 << 20}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write appends e, setting the time when it is empty. A failure is reported
// on stderr and not returned: the audited action has already happened and
// must not fail because of its record.
func (l *Log) Write(e Entry) {
	if l == nil {
		return
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep commands such as "show <x>" readable
	if err := enc.Encode(e); err != nil {
		log.Printf("audit: %v", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size > 0 && l.size+int64(buf.Len()) > l.maxSize {
		l.f.Close()
		l.f = nil
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			log.Printf("audit: rotate %s: %v", l.path, err)
		}
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			log.Printf("audit: %v", err)
			return
		}
	}
	n, err := l.f.Write(buf.Bytes())
	l.size += int64(n)
	if err != nil {
		log.Printf("audit: write %s: %v", l.path, err)
	}
}

// Read returns at most limit entries, newest first, with a value containing
// q (case-insensitive; empty matches all). It reads the current file and, when
// that holds too few, the rotated one. Lines that do not parse are skipped.
func (l *Log) Read(limit int, q string) ([]Entry, error) {
	out := []Entry{}
	if l == nil {
		return out, nil
	}
	q = strings.ToLower(q)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, path := range []string{l.path, l.path + ".1"} {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		lines := bytes.Split(data, []byte("\n"))
		for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
			var e Entry
			if len(lines[i]) == 0 || json.Unmarshal(lines[i], &e) != nil {
				continue
			}
			if q == "" || e.matches(q) {
				out = append(out, e)
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// matches looks at the values only, so a search for "user" does not match
// every line through its key.
func (e Entry) matches(q string) bool {
	for _, v := range []string{e.Time, e.User, e.IP, e.Action, e.Target, e.Detail, e.Result, e.Error} {
		if strings.Contains(strings.ToLower(v), q) {
			return true
		}
	}
	return false
}

// Close closes the file; a later Write reopens it.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
