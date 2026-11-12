package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"fsdeck/internal/esl"
	"fsdeck/internal/fsapi"
	"fsdeck/internal/store"
)

// instanceView is what the API exposes of an instance: never the password.
type instanceView struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Host  string    `json:"host"`
	Port  int       `json:"port"`
	State esl.State `json:"state"`
}

type instanceRequest struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
}

func view(in store.Instance, inst *instance) instanceView {
	v := instanceView{ID: in.ID, Name: in.Name, Host: in.Host, Port: in.Port}
	if inst != nil {
		v.State = inst.sw.State()
	}
	return v
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "instance not found")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "cannot save instances: "+err.Error())
	}
}

func (s *Server) handleListInstances(w http.ResponseWriter, _ *http.Request, _ user) {
	list := s.cfg.Store.List()
	views := make([]instanceView, 0, len(list))
	s.mu.Lock()
	for _, in := range list {
		views = append(views, view(in, s.insts[in.ID]))
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request, _ user) {
	var req instanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	in, err := s.cfg.Store.Add(store.Instance{Name: req.Name, Host: req.Host, Port: req.Port, Password: req.Password})
	if err != nil {
		s.mu.Unlock()
		writeStoreError(w, err)
		return
	}
	inst := newInstance(s.dial(in))
	s.insts[in.ID] = inst
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, view(in, inst))
}

func (s *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request, _ user) {
	var req instanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	err := s.cfg.Store.Update(store.Instance{ID: id, Name: req.Name, Host: req.Host, Port: req.Port, Password: req.Password})
	if err != nil {
		s.mu.Unlock()
		writeStoreError(w, err)
		return
	}
	in, _ := s.cfg.Store.Get(id)
	old := s.insts[id]
	inst := newInstance(s.dial(in))
	s.insts[id] = inst
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	writeJSON(w, http.StatusOK, view(in, inst))
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request, _ user) {
	id := r.PathValue("id")
	s.mu.Lock()
	if err := s.cfg.Store.Delete(id); err != nil {
		s.mu.Unlock()
		writeStoreError(w, err)
		return
	}
	old := s.insts[id]
	delete(s.insts, id)
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	resp := struct {
		State    esl.State       `json:"state"`
		Hostname string          `json:"hostname"`
		Status   json.RawMessage `json:"status"` // null while disconnected
	}{State: inst.sw.State()}
	if resp.State.Connected {
		ctx, cancel := context.WithTimeout(r.Context(), eslTimeout)
		defer cancel()
		var err error
		if resp.Hostname, err = fsapi.Hostname(ctx, inst.sw); err == nil {
			resp.Status, err = fsapi.Status(ctx, inst.sw)
		}
		if err != nil {
			writeAPIError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.connected(w, r)
	if inst == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), eslTimeout)
	defer cancel()
	name := r.PathValue("name")
	doc, err := fsapi.Config(ctx, inst.sw, name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "xml": doc})
}

func (s *Server) handleHangup(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.connected(w, r)
	if inst == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), eslTimeout)
	defer cancel()
	writeOutput(w)(fsapi.HangupChannel(ctx, inst.sw, r.PathValue("uuid")))
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	var req struct {
		Action  string `json:"action"`
		Profile string `json:"profile"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var run func(context.Context, fsapi.Commander) (string, error)
	switch req.Action {
	case "reloadxml":
		run = fsapi.ReloadXML
	case "reloadacl":
		run = fsapi.ReloadACL
	case "sofia_rescan":
		run = func(ctx context.Context, c fsapi.Commander) (string, error) {
			return fsapi.SofiaRescan(ctx, c, req.Profile)
		}
	default:
		writeError(w, http.StatusBadRequest, "unknown action")
		return
	}
	if !ready(w, inst) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), eslTimeout)
	defer cancel()
	writeOutput(w)(run(ctx, inst.sw))
}

// handleConsole returns the raw reply, "-ERR" included.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	cmd := strings.TrimSpace(req.Command)
	switch {
	case cmd == "":
		writeError(w, http.StatusBadRequest, "command is required")
		return
	case strings.ContainsAny(cmd, "\r\n"):
		writeError(w, http.StatusBadRequest, "command must be a single line")
		return
	}
	if !ready(w, inst) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), consoleTimeout)
	defer cancel()
	out, err := inst.sw.API(ctx, cmd)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
}

func writeOutput(w http.ResponseWriter) func(string, error) {
	return func(out string, err error) {
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"output": out})
	}
}
