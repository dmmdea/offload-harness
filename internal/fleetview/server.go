package fleetview

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed ui.html
var uiHTML []byte

// NewHandler serves the read-only operator overview: the embedded page at
// "/", the live Overview snapshot as JSON at "/api/overview", and a plain
// health check at "/healthz". It never issues any outbound request itself —
// all polling happens in the Poller this handler only reads from.
func NewHandler(p *Poller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(uiHTML)
	})
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(p.Snapshot())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}
