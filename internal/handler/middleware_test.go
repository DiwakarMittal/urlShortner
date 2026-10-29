package handler

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The recorder must do BOTH jobs: remember the status and forward it.
func TestStatusRecorder_SavesAndForwards(t *testing.T) {
	under := httptest.NewRecorder() // the "real" writer underneath
	rec := &statusRecorder{ResponseWriter: under, status: http.StatusOK}

	rec.WriteHeader(http.StatusTeapot)

	if rec.status != http.StatusTeapot {
		t.Errorf("saved status = %d, want %d", rec.status, http.StatusTeapot)
	}
	if under.Code != http.StatusTeapot {
		t.Errorf("forwarded status = %d, want %d", under.Code, http.StatusTeapot)
	}
}

// captureLog redirects the standard logger into a buffer for one test.
// It changes global state, so don't combine it with t.Parallel().
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) }) // restore after the test
	return &buf
}

func TestLogRequests_LogsExplicitStatus(t *testing.T) {
	buf := captureLog(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	LogRequests(next).ServeHTTP(rec, httptest.NewRequest("GET", "/brew", nil))

	// The middleware must not change the response...
	if rec.Code != http.StatusTeapot {
		t.Errorf("response status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	// ...and must log method, path and status.
	if got := buf.String(); !strings.Contains(got, "GET /brew 418") {
		t.Errorf("log = %q, want it to contain %q", got, "GET /brew 418")
	}
}

// A handler that never calls WriteHeader gets an implicit 200 from Go.
// This is the only case where the recorder's default value matters.
func TestLogRequests_DefaultsTo200(t *testing.T) {
	buf := captureLog(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello")) // body only, no WriteHeader
	})

	rec := httptest.NewRecorder()
	LogRequests(next).ServeHTTP(rec, httptest.NewRequest("GET", "/hi", nil))

	if got := buf.String(); !strings.Contains(got, "GET /hi 200") {
		t.Errorf("log = %q, want it to contain %q", got, "GET /hi 200")
	}
}

// The query string must never reach the log (it can contain secrets).
func TestLogRequests_OmitsQueryString(t *testing.T) {
	buf := captureLog(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	rec := httptest.NewRecorder()
	LogRequests(next).ServeHTTP(rec, httptest.NewRequest("GET", "/x?token=secret123", nil))

	if strings.Contains(buf.String(), "secret123") {
		t.Errorf("log leaked the query string: %q", buf.String())
	}
}

// Health checks arrive every few seconds; they must not fill the log.
func TestLogRequests_SkipsHealthChecks(t *testing.T) {
	buf := captureLog(t)
	h := LogRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, path := range []string{"/healthz", "/readyz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	if buf.Len() != 0 {
		t.Errorf("health checks were logged: %q", buf.String())
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/abc", nil))
	if !strings.Contains(buf.String(), "GET /abc") {
		t.Errorf("normal request not logged: %q", buf.String())
	}
}
