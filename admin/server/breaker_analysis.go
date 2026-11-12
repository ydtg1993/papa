package server

import "net/http"

func (s *Monitor) breakerAnalysisHandler(w http.ResponseWriter, r *http.Request) {
	s.breakers.Analysis(w, r)
}
