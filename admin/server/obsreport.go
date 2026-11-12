package server

import (
	"github.com/ydtg1993/papa/v4/admin/reports"
	"net/http"
)

const obsReportMaxFileBytes = reports.MaxFileBytes

func (s *Monitor) obsReportListHandler(w http.ResponseWriter, r *http.Request) { s.reports.List(w, r) }
func (s *Monitor) obsReportFileHandler(w http.ResponseWriter, r *http.Request) { s.reports.File(w, r) }
