package server

import (
	"net/http"
)

// Auth applies source restrictions and token verification to every path.
// Hosts use this for extension APIs, including paths outside /api/.
func (s *Monitor) Auth(next http.Handler) http.Handler { return s.access.Auth(next) }

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// NoCache disables browser caching for host-mounted static resources.
func NoCache(next http.Handler) http.Handler { return noCache(next) }
