package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/juki/fsdeck/internal/audit"
	"github.com/juki/fsdeck/internal/esl"
	"github.com/juki/fsdeck/internal/fsapi"
	"github.com/juki/fsdeck/internal/store"
)

// instanceView is what the API exposes of an instance: never the ESL
// password or the agent token.
type instanceView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	State      esl.State `json:"state"`
	ConfigMode string    `json:"configMode"`
	ConfigPath string    `json:"configPath"`
	AgentURL   string    `json:"agentURL"`
}

// instanceRequest is the body of instance create and update. Without
// configMode the config access stays as it is; an empty password or
// agentToken keeps the stored one.
type instanceRequest struct {
	Name       string  `json:"name"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	Password   string  `json:"password"`
	ConfigMode *string `json:"configMode"`
	ConfigPath string  `json:"configPath"`
	AgentURL   string  `json:"agentURL"`
	AgentToken string  `json:"agentToken"`
}

// instance builds the stored instance; old supplies the config access when
// the request does not set configMode.
func (req instanceRequest) instance(id string, old store.Instance) store.Instance {
	in := store.Instance{ID: id, Name: req.Name, Host: req.Host, Port: req.Port, Password: req.Password,
		ConfigMode: old.ConfigMode, ConfigPath: old.ConfigPath, AgentURL: old.AgentURL, AgentToken: old.AgentToken}
	if req.ConfigMode != nil {
		in.ConfigMode, in.ConfigPath, in.AgentURL, in.AgentToken = *req.ConfigMode, req.ConfigPath, req.AgentURL, req.AgentToken
	}
	return in
}

func view(in store.Instance, inst *instance) instanceView {
	v := instanceView{ID: in.ID, Name: in.Name, Host: in.Host, Port: in.Port, ConfigMode: in.ConfigMode, ConfigPath: in.ConfigPath, AgentURL: in.AgentURL}
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

// instanceEntry starts the audit entry of an instance change; id is empty
// when a create failed.
func instanceEntry(u user, action, id string, req instanceRequest) audit.Entry {
	detail := fmt.Sprintf("host=%s port=%d", truncate(req.Host, 256), req.Port)
	if id != "" {
		detail = "id=" + id + " " + detail
	}
	if req.Password != "" {
		detail += " password=changed"
	}
	if req.ConfigMode != nil {
		detail += " config=" + truncate(*req.ConfigMode, 16)
		if req.AgentToken != "" {
			detail += " agentToken=changed"
		}
	}
	return audit.Entry{User: u.Username, Action: action, Target: truncate(req.Name, 64), Detail: detail}
}

// record completes e with the outcome err and writes it to the audit log.
func (s *Server) record(r *http.Request, e audit.Entry, err error) {
	e.Result = audit.OK
	if err != nil {
		e.Result, e.Error = audit.Error, err.Error()
	}
	s.audit(r, e)
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request, u user) {
	var req instanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	in, err := s.cfg.Store.Add(req.instance("", store.Instance{}))
	if err != nil {
		s.mu.Unlock()
		s.record(r, instanceEntry(u, "instance.create", "", req), err)
		writeStoreError(w, err)
		return
	}
	inst := s.newInstance(in)
	s.insts[in.ID] = inst
	s.mu.Unlock()
	s.record(r, instanceEntry(u, "instance.create", in.ID, req), nil)
	writeJSON(w, http.StatusCreated, view(in, inst))
}

func (s *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request, u user) {
	var req instanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	entry := instanceEntry(u, "instance.update", id, req)
	s.mu.Lock()
	prev, _ := s.cfg.Store.Get(id)
	err := s.cfg.Store.Update(req.instance(id, prev))
	if err != nil {
		s.mu.Unlock()
		s.record(r, entry, err)
		writeStoreError(w, err)
		return
	}
	in, _ := s.cfg.Store.Get(id)
	// An update reconnects, as it always has, unless it changes nothing but
	// the config access: then the connection, its history and its log
	// backlog stay.
	old, inst := s.insts[id], s.insts[id]
	if inst == nil || in.Name != prev.Name || in.Host != prev.Host || in.Port != prev.Port || req.Password != "" {
		inst = s.newInstance(in)
		s.insts[id] = inst
	} else {
		old = nil
	}
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	s.record(r, entry, nil)
	writeJSON(w, http.StatusOK, view(in, inst))
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request, u user) {
	id := r.PathValue("id")
	entry := audit.Entry{User: u.Username, Action: "instance.delete", Target: s.instanceName(id), Detail: "id=" + truncate(id, 64)}
	s.mu.Lock()
	if err := s.cfg.Store.Delete(id); err != nil {
		s.mu.Unlock()
		s.record(r, entry, err)
		writeStoreError(w, err)
		return
	}
	old := s.insts[id]
	delete(s.insts, id)
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	s.record(r, entry, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) instanceName(id string) string {
	in, _ := s.cfg.Store.Get(id)
	return in.Name
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
		ctx, cancel := eslContext(r, eslTimeout)
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
	ctx, cancel := eslContext(r, eslTimeout)
	defer cancel()
	name := r.PathValue("name")
	doc, err := fsapi.Config(ctx, inst.sw, name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "xml": doc})
}

func (s *Server) handleHangup(w http.ResponseWriter, r *http.Request, u user) {
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	uuid := r.PathValue("uuid")
	entry := audit.Entry{User: u.Username, Action: "channel.hangup", Target: s.instanceName(r.PathValue("id")), Detail: "uuid=" + truncate(uuid, 128)}
	if !ready(w, inst) {
		s.record(r, entry, esl.ErrNotConnected)
		return
	}
	ctx, cancel := eslContext(r, eslTimeout)
	defer cancel()
	out, err := fsapi.HangupChannel(ctx, inst.sw, uuid)
	s.record(r, entry, err)
	writeOutput(w)(out, err)
}

// handleConsole returns the raw reply, "-ERR" included. The audit entry
// carries the command text; the audit log is admin-only, like the console.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request, u user) {
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
	entry := audit.Entry{User: u.Username, Action: "console.run", Target: s.instanceName(r.PathValue("id")), Detail: cmd}
	if !ready(w, inst) {
		s.record(r, entry, esl.ErrNotConnected)
		return
	}
	ctx, cancel := eslContext(r, consoleTimeout)
	defer cancel()
	out, err := inst.sw.API(ctx, cmd)
	if err != nil {
		s.record(r, entry, err)
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if strings.HasPrefix(out, "-ERR") {
		first, _, _ := strings.Cut(out, "\n")
		err = errors.New(truncate(strings.TrimSpace(first), 200))
	}
	s.record(r, entry, err)
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
}

// handleComplete answers a Tab in the console. It runs on every key press,
// so it is not audited.
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	var req struct {
		Line string `json:"line"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := fsapi.CheckCompleteLine(req.Line); err != nil {
		writeAPIError(w, err)
		return
	}
	if !ready(w, inst) {
		return
	}
	ctx, cancel := eslContext(r, eslTimeout)
	defer cancel()
	c, err := fsapi.Complete(ctx, inst.sw, req.Line)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.connected(w, r)
	if inst == nil {
		return
	}
	ctx, cancel := eslContext(r, eslTimeout)
	defer cancel()
	rows, err := fsapi.APICommands(ctx, inst.sw)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
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
