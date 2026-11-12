//go:build community

package server

import "net/http"

// A community build contains none of the paid features.
const paidBuild = false

// registerPaid has nothing to register in a community build.
func (s *Server) registerPaid(*http.ServeMux) {}
