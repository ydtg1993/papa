package server

import (
	"github.com/ydtg1993/papa/v4/admin/logs"
	"io"
	"net/http"
)

const logTailLines = logs.TailLines
const logTailBytes = logs.TailBytes

type logTail = logs.TailResult

func isLogFileName(name string) bool                                      { return logs.IsFileName(name) }
func readLogTail(r io.ReaderAt, size int64) (logTail, error)              { return logs.ReadTail(r, size) }
func (s *Monitor) logsTailHandler(w http.ResponseWriter, r *http.Request) { s.logs.Tail(w, r) }
