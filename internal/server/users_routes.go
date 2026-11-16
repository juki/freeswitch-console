package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juki/fsdeck/internal/audit"
	"github.com/juki/fsdeck/internal/store"
)

// userView is what the API exposes of an account: never the hash.
type userView struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	Role      string     `json:"role"`
	Disabled  bool       `json:"disabled"`
	Managed   bool       `json:"managed"`
	CreatedAt *time.Time `json:"createdAt"` // null for a managed account
}

func viewUser(a account) userView {
	v := userView{ID: a.ID, Username: a.Username, Role: a.Role, Disabled: a.Disabled, Managed: a.Managed}
	if !a.Managed {
		t := a.CreatedAt
		v.CreatedAt = &t
	}
	return v
}

func (s *Server) handleListUsers(w http.ResponseWriter, _ *http.Request, _ user) {
	accs := s.accounts()
	views := make([]userView, 0, len(accs))
	for _, a := range accs {
		views = append(views, viewUser(a))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, me user) {
	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	entry := audit.Entry{User: me.Username, Action: "user.create", Target: truncate(req.Username, 64), Detail: "role=" + truncate(req.Role, 16)}
	fail := func(code int, msg string) {
		entry.Result, entry.Error = audit.Error, msg
		s.audit(r, entry)
		writeError(w, code, msg)
	}
	err := store.CheckUsername(req.Username)
	if err == nil {
		err = store.CheckRole(req.Role)
	}
	if err == nil {
		err = checkPassword(req.Password)
	}
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if s.isManagedName(req.Username) {
		fail(http.StatusConflict, store.ErrDuplicate.Error())
		return
	}
	if !s.acquireVerify(r) {
		writeBusy(w)
		return
	}
	hash, err := hashPassword(req.Password)
	s.releaseVerify()
	if err != nil {
		fail(http.StatusInternalServerError, "cannot hash the password")
		return
	}
	s.usersMu.Lock()
	u, err := s.cfg.Store.AddUser(store.User{Username: req.Username, Role: req.Role, PasswordHash: hash})
	s.usersMu.Unlock()
	if err != nil {
		entry.Result, entry.Error = audit.Error, err.Error()
		s.audit(r, entry)
		writeUserStoreError(w, err)
		return
	}
	entry.Detail = "id=" + u.ID + " " + entry.Detail
	entry.Result = audit.OK
	s.audit(r, entry)
	writeJSON(w, http.StatusCreated, viewUser(account{User: u}))
}

// handleUpdateUser changes role, disabled and password; each is optional and
// an empty password keeps the current one.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, me user) {
	var req struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	acc, ok := s.accountByID(id)
	if !ok {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	entry := audit.Entry{User: me.Username, Action: "user.update", Target: acc.Username}
	var changes []string
	if req.Role != nil {
		changes = append(changes, "role="+truncate(*req.Role, 16))
	}
	if req.Disabled != nil {
		changes = append(changes, "disabled="+strconv.FormatBool(*req.Disabled))
	}
	newPassword := req.Password != nil && *req.Password != ""
	if newPassword {
		changes = append(changes, "password=changed")
	}
	entry.Detail = strings.Join(append([]string{"id=" + id}, changes...), " ")
	refuse := func(code int, result, msg string) {
		entry.Result, entry.Error = result, msg
		s.audit(r, entry)
		writeError(w, code, msg)
	}
	if acc.Managed {
		refuse(http.StatusForbidden, audit.Denied, errManaged)
		return
	}
	if req.Role != nil {
		if err := store.CheckRole(*req.Role); err != nil {
			refuse(http.StatusBadRequest, audit.Error, err.Error())
			return
		}
	}
	var hash string
	if newPassword {
		if err := checkPassword(*req.Password); err != nil {
			refuse(http.StatusBadRequest, audit.Error, err.Error())
			return
		}
		if !s.acquireVerify(r) {
			writeBusy(w)
			return
		}
		var err error
		hash, err = hashPassword(*req.Password)
		s.releaseVerify()
		if err != nil {
			refuse(http.StatusInternalServerError, audit.Error, "cannot hash the password")
			return
		}
	}

	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	cur, ok := s.cfg.Store.User(id)
	if !ok {
		refuse(http.StatusNotFound, audit.Error, "user not found")
		return
	}
	next := cur
	if req.Role != nil {
		next.Role = *req.Role
	}
	if req.Disabled != nil {
		next.Disabled = *req.Disabled
	}
	if hash != "" {
		next.PasswordHash = hash
	}
	if msg := s.guardAdmin(me, cur, next.Role == store.RoleAdmin && !next.Disabled); msg != "" {
		refuse(http.StatusConflict, audit.Denied, msg)
		return
	}
	if err := s.cfg.Store.UpdateUser(next); err != nil {
		entry.Result, entry.Error = audit.Error, err.Error()
		s.audit(r, entry)
		writeUserStoreError(w, err)
		return
	}
	if next.Role != cur.Role || next.Disabled != cur.Disabled || hash != "" {
		// Changing one's own password keeps the session it was done from.
		keep := ""
		if id == me.ID {
			keep = sessionToken(r)
		}
		s.endSessions(id, keep, hash)
	}
	entry.Result = audit.OK
	s.audit(r, entry)
	writeJSON(w, http.StatusOK, viewUser(account{User: next}))
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, me user) {
	id := r.PathValue("id")
	acc, ok := s.accountByID(id)
	if !ok {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	entry := audit.Entry{User: me.Username, Action: "user.delete", Target: acc.Username, Detail: "id=" + id}
	refuse := func(code int, msg string) {
		entry.Result, entry.Error = audit.Denied, msg
		s.audit(r, entry)
		writeError(w, code, msg)
	}
	if acc.Managed {
		refuse(http.StatusForbidden, errManaged)
		return
	}
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	if msg := s.guardAdmin(me, acc.User, false); msg != "" {
		refuse(http.StatusConflict, msg)
		return
	}
	if err := s.cfg.Store.DeleteUser(id); err != nil {
		entry.Result, entry.Error = audit.Error, err.Error()
		s.audit(r, entry)
		writeUserStoreError(w, err)
		return
	}
	s.endSessions(id, "", "")
	entry.Result = audit.OK
	s.audit(r, entry)
	w.WriteHeader(http.StatusNoContent)
}

// guardAdmin refuses to delete, disable or demote the signed-in account, and
// to leave no enabled admin. stillAdmin tells whether cur remains an enabled
// admin after the change. usersMu must be held.
func (s *Server) guardAdmin(me user, cur store.User, stillAdmin bool) string {
	wasAdmin := cur.Role == store.RoleAdmin && !cur.Disabled
	if !wasAdmin || stillAdmin {
		return ""
	}
	if cur.ID == me.ID {
		return "you cannot delete, disable or demote the account you are signed in with"
	}
	if s.enabledAdmins() <= 1 {
		return "the last enabled admin cannot be deleted, disabled or demoted"
	}
	return ""
}
