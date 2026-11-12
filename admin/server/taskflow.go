package server

import "net/http"

func (s *Monitor) taskFlowHandler(w http.ResponseWriter, r *http.Request) { s.taskflow.ServeHTTP(w, r) }
