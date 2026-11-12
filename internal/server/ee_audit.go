//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package server

import (
	"fmt"
	"net/http"
	"strconv"
)

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, _ user) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, 1000)
	}
	rows, err := s.cfg.Audit.Read(limit, r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("cannot read the audit log: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.cfg.Audit != nil, "rows": rows})
}
