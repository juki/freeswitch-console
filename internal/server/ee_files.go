//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package server

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"fsdeck/ee/conffs"
	"fsdeck/internal/audit"
	"fsdeck/internal/license"
	"fsdeck/internal/store"
)

// filesTimeout bounds one config file operation; the agent client has its
// own, shorter timeout.
const filesTimeout = 30 * time.Second

// registerFiles adds the config file routes: admin only, since raw files
// hold secrets that cannot be masked, and only with the config-edit
// licence.
func (s *Server) registerFiles(m *http.ServeMux) {
	f := license.FeatureConfigEdit
	p := "/api/instances/{id}/files"
	s.handlePaid(m, "GET "+p, capFiles, f, "config.list", s.handleFilesList)
	s.handlePaid(m, "GET "+p+"/content", capFiles, f, "config.read", s.handleFileRead)
	s.handlePaid(m, "PUT "+p+"/content", capFiles, f, "config.write", s.handleFileWrite)
	s.handlePaid(m, "DELETE "+p+"/content", capFiles, f, "config.delete", s.handleFileDelete)
	s.handlePaid(m, "GET "+p+"/history", capFiles, f, "config.history", s.handleFileHistory)
	s.handlePaid(m, "GET "+p+"/backup", capFiles, f, "config.backup", s.handleFileBackup)
	s.handlePaid(m, "POST "+p+"/restore", capFiles, f, "config.restore", s.handleFileRestore)
	s.handlePaid(m, "GET "+p+"/search", capFiles, f, "config.search", s.handleFileSearch)
}

// locals holds one Local per configuration and backup directory, so writers
// in this process share its lock.
var locals = struct {
	sync.Mutex
	m map[[2]string]*conffs.Local
}{m: map[[2]string]*conffs.Local{}}

// fileStore returns the config access of the instance in the path, or
// answers 404 or 409 and returns nil.
func (s *Server) fileStore(w http.ResponseWriter, r *http.Request) (conffs.FileStore, store.Instance) {
	in, ok := s.cfg.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "instance not found")
		return nil, in
	}
	switch in.ConfigMode {
	case store.ConfigLocal:
		if s.cfg.BackupDir == "" {
			writeError(w, http.StatusInternalServerError, "no backup directory is configured")
			return nil, in
		}
		key := [2]string{in.ConfigPath, filepath.Join(s.cfg.BackupDir, in.ID)}
		locals.Lock()
		defer locals.Unlock()
		l := locals.m[key]
		if l == nil {
			var err error
			if l, err = conffs.NewLocal(key[0], key[1]); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return nil, in
			}
			locals.m[key] = l
		}
		return l, in
	case store.ConfigAgent:
		c, err := conffs.NewClient(in.AgentURL, in.AgentToken)
		if err != nil {
			conffs.WriteError(w, err)
			return nil, in
		}
		return c, in
	}
	writeError(w, http.StatusConflict, "config access is not set up for this instance")
	return nil, in
}

func filesContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), filesTimeout)
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request, _ user) {
	fs, _ := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	rows, err := fs.List(ctx, r.URL.Query().Get("path"))
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request, _ user) {
	fs, _ := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	f, err := fs.Read(ctx, r.URL.Query().Get("path"))
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleFileHistory(w http.ResponseWriter, r *http.Request, _ user) {
	fs, _ := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	rows, err := fs.History(ctx, r.URL.Query().Get("path"))
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Server) handleFileBackup(w http.ResponseWriter, r *http.Request, _ user) {
	fs, _ := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	q := r.URL.Query()
	b, err := fs.Backup(ctx, q.Get("path"), q.Get("id"))
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// fileEntry starts the audit entry of a change; detail names the path and
// the hashes, never the content.
func fileEntry(u user, action string, in store.Instance, path string) audit.Entry {
	return audit.Entry{User: u.Username, Action: action, Target: in.Name, Detail: "path=" + truncate(path, 256)}
}

func baseText(sha string) string {
	if sha == "" {
		return "new"
	}
	return truncate(sha, 64)
}

// recordChange completes e and answers the change.
func (s *Server) recordChange(w http.ResponseWriter, r *http.Request, e audit.Entry, res conffs.Written, err error) {
	if err == nil {
		e.Detail += " sha256=" + res.SHA256
	}
	s.record(r, e, err)
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		BaseSHA256 string `json:"baseSha256"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	fs, in := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	e := fileEntry(u, "config.write", in, req.Path)
	e.Detail += " base=" + baseText(req.BaseSHA256)
	res, err := fs.Write(ctx, req.Path, req.Content, req.BaseSHA256, u.Username)
	s.recordChange(w, r, e, res, err)
}

func (s *Server) handleFileRestore(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Path       string `json:"path"`
		ID         string `json:"id"`
		BaseSHA256 string `json:"baseSha256"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	fs, in := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	e := fileEntry(u, "config.restore", in, req.Path)
	e.Detail += " id=" + truncate(req.ID, 64) + " base=" + baseText(req.BaseSHA256)
	res, err := fs.Restore(ctx, req.Path, req.ID, req.BaseSHA256, u.Username)
	s.recordChange(w, r, e, res, err)
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request, u user) {
	fs, in := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	q := r.URL.Query()
	e := fileEntry(u, "config.delete", in, q.Get("path"))
	e.Detail += " base=" + baseText(q.Get("baseSha256"))
	err := fs.Delete(ctx, q.Get("path"), q.Get("baseSha256"), u.Username)
	s.record(r, e, err)
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFileSearch is not audited when allowed: it only reads.
func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request, _ user) {
	q, opts, err := conffs.ParseSearch(r.URL.Query())
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	fs, _ := s.fileStore(w, r)
	if fs == nil {
		return
	}
	ctx, cancel := filesContext(r)
	defer cancel()
	res, err := fs.Search(ctx, q, opts)
	if err != nil {
		conffs.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
