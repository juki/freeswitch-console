package server

import (
	"net/http"
	"time"

	"fsdeck/internal/license"
	"fsdeck/internal/store"
)

// LicenseInfo is the answer of GET /api/license.
type LicenseInfo struct {
	Edition  string     `json:"edition"` // community or enterprise
	Status   string     `json:"status"`  // none, valid, expired or invalid
	Licensee string     `json:"licensee"`
	Plan     string     `json:"plan"`
	Features []string   `json:"features"`
	Expires  *time.Time `json:"expires"` // null without a licence or for one that never expires
}

// License describes the edition the server runs as. A community build has
// no paid features and reports no licence whatever key it was given.
func (s *Server) License() LicenseInfo {
	info := LicenseInfo{Edition: "community", Status: "none", Features: []string{}}
	if !paidBuild {
		return info
	}
	now := time.Now()
	info.Status = s.cfg.License.Status(now)
	if info.Status == "valid" {
		info.Edition = "enterprise"
	}
	if l, ok := s.cfg.License.License(); ok {
		info.Licensee, info.Plan, info.Features = l.Licensee, l.Plan, append([]string{}, l.Features...)
		if l.Expires != 0 {
			t := time.Unix(l.Expires, 0).UTC()
			info.Expires = &t
		}
	}
	return info
}

func (s *Server) handleLicense(w http.ResponseWriter, _ *http.Request, _ user) {
	writeJSON(w, http.StatusOK, s.License())
}

// has reports whether the licence grants feature now.
func (s *Server) has(feature string) bool {
	return paidBuild && s.cfg.License.Has(feature, time.Now())
}

// handlePaid registers a route of a paid feature. Without a licence for
// feature it answers 402, after the session and role checks.
func (s *Server) handlePaid(m *http.ServeMux, pattern string, c capability, feature, action string, h func(http.ResponseWriter, *http.Request, user)) {
	s.routeFeatures[pattern] = feature
	s.handle(m, pattern, c, action, func(w http.ResponseWriter, r *http.Request, u user) {
		if !s.has(feature) {
			writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "this feature needs a licence", "feature": feature})
			return
		}
		h(w, r, u)
	})
}

// errUnlicensedAccount is the sign-in answer for an account the licence
// does not cover.
const errUnlicensedAccount = "without a licence for the users feature only the accounts admin (role admin) and viewer (role viewer) can sign in"

// licensed reports whether acc may sign in. Without the users feature only
// admin with role admin and viewer with role viewer may.
func (s *Server) licensed(acc account) bool {
	if s.has(license.FeatureUsers) {
		return true
	}
	return acc.Username == "admin" && acc.Role == store.RoleAdmin || acc.Username == "viewer" && acc.Role == store.RoleViewer
}
