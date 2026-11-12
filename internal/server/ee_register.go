//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package server

import (
	"net/http"

	"fsdeck/internal/license"
	"fsdeck/internal/store"
)

const paidBuild = true

// The operator role belongs to the paid user management.
func init() {
	roleCaps[store.RoleOperator] = map[capability]bool{capRead: true, capLogs: true, capOperate: true}
}

// registerPaid adds the routes of the paid features. Listing the accounts
// stays open so a Community admin sees them; changing them needs a licence.
func (s *Server) registerPaid(m *http.ServeMux) {
	s.handle(m, "GET /api/users", capUsers, "user.list", s.handleListUsers)
	s.handlePaid(m, "POST /api/users", capUsers, license.FeatureUsers, "user.create", s.handleCreateUser)
	s.handlePaid(m, "PUT /api/users/{id}", capUsers, license.FeatureUsers, "user.update", s.handleUpdateUser)
	s.handlePaid(m, "DELETE /api/users/{id}", capUsers, license.FeatureUsers, "user.delete", s.handleDeleteUser)
	s.handlePaid(m, "GET /api/audit", capUsers, license.FeatureAudit, "audit.read", s.handleAudit)
	s.registerFiles(m)
}
