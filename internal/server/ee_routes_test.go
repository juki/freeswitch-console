//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package server

import (
	"testing"

	"fsdeck/internal/license"
)

// paidRoutes are the routes of the paid features.
func paidRoutes(id string) []routeCase {
	unknownUser := "/api/users/0000000000000000"
	files := "/api/instances/" + id + "/files"
	return []routeCase{
		{"GET /api/users", "GET", "/api/users", "", capUsers, ""},
		{"POST /api/users", "POST", "/api/users", `{"username":"new.user","role":"viewer","password":"` + userPW + `"}`, capUsers, license.FeatureUsers},
		{"PUT /api/users/{id}", "PUT", unknownUser, `{"role":"viewer"}`, capUsers, license.FeatureUsers},
		{"DELETE /api/users/{id}", "DELETE", unknownUser, "", capUsers, license.FeatureUsers},
		{"GET /api/audit", "GET", "/api/audit", "", capUsers, license.FeatureAudit},
		// The harness instance has no config access: 409 once past the checks.
		{"GET /api/instances/{id}/files", "GET", files + "?path=", "", capFiles, license.FeatureConfigEdit},
		{"GET /api/instances/{id}/files/content", "GET", files + "/content?path=a.xml", "", capFiles, license.FeatureConfigEdit},
		{"PUT /api/instances/{id}/files/content", "PUT", files + "/content", `{"path":"a.xml","content":"<a/>","baseSha256":""}`, capFiles, license.FeatureConfigEdit},
		{"DELETE /api/instances/{id}/files/content", "DELETE", files + "/content?path=a.xml&baseSha256=x", "", capFiles, license.FeatureConfigEdit},
		{"GET /api/instances/{id}/files/history", "GET", files + "/history?path=a.xml", "", capFiles, license.FeatureConfigEdit},
		{"GET /api/instances/{id}/files/backup", "GET", files + "/backup?path=a.xml&id=x", "", capFiles, license.FeatureConfigEdit},
		{"GET /api/instances/{id}/files/search", "GET", files + "/search?q=ab", "", capFiles, license.FeatureConfigEdit},
		{"POST /api/instances/{id}/files/restore", "POST", files + "/restore", `{"path":"a.xml","id":"x","baseSha256":""}`, capFiles, license.FeatureConfigEdit},
	}
}

func matrixRuns(t *testing.T) []matrixRun {
	return []matrixRun{
		{name: "licensed", opts: []func(*Config){licensed(t)}, features: true},
		{name: "unlicensed"},
	}
}
