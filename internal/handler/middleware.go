package handler

import (
	"log"
	"net/http"
	"time"
)

// statusRecorder wraps http.ResponseWriter to remember the status code.
// Embedding the interface (no field name) gives statusRecorder all of its
// methods for free; we only override the one we need to observe.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader is called by handlers when they choose a status code.
// We save it for the log line, then forward the call so the client still
// receives that status. Skipping the forward would break every response.
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// LogRequests wraps next and logs one line per request:
// method, path, status, duration.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Default 200: if a handler writes a body without ever calling
		// WriteHeader, Go sends 200 on its behalf and we never see a call.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Pass rec, NOT w. Otherwise the handler writes to the original
		// writer and our recorder never learns the status.
		next.ServeHTTP(rec, r)

		// Docker polls these every few seconds; logging them would bury
		// real traffic.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}

		// r.URL.Path only: no query string or body, which can hold secrets.
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}
