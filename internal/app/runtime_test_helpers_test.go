package app

import (
	"encoding/json"
	"net/http"
)

// Synthetic browser endpoints use the same wire shape as production adapters.
func writeRuntimeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
