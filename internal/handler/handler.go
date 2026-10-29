package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"urlshortener/internal/store"
)

// StatsRecorder counts a visit for analytics without blocking the redirect.
type StatsRecorder interface {
	Record(code string, at time.Time, referer, userAgent string)
}

// StatsReader serves GET /stats/{code}; *store.PostgresStore satisfies it.
type StatsReader interface {
	Stats(ctx context.Context, code string, since time.Time) (store.Stats, error)
}

type noStats struct{}

func (noStats) Record(string, time.Time, string, string) {}

// Handler holds what the HTTP handlers need. It depends on the Store
// interface, never on Postgres, so the database could be swapped or faked.
type Handler struct {
	store       store.Store
	clicks      ClickRecorder
	baseURL     string // public address of this server, e.g. "http://localhost:8080"
	shortenMW   Middleware
	redirectMW  Middleware
	stats       StatsRecorder
	statsReader StatsReader // nil = no /stats route
	statsToken  string      // non-empty = /stats needs "Authorization: Bearer <token>"
}

// ClickRecorder counts a visit without blocking the redirect. The real
// implementation (clicks.Counter) aggregates in memory; tests use a fake.
type ClickRecorder interface {
	Record(code string)
}

// NewHandler is the constructor. TrimRight removes a trailing "/" so we
// never build URLs like "http://host//abc". Both routes start unlimited.
func NewHandler(s store.Store, c ClickRecorder, baseURL string) *Handler {
	return &Handler{
		store:      s,
		clicks:     c,
		baseURL:    strings.TrimRight(baseURL, "/"),
		shortenMW:  noMiddleware,
		redirectMW: noMiddleware,
		stats:      noStats{},
	}
}

// UseStats turns on analytics: rec receives every successful redirect,
// reader (may be nil) serves GET /stats/{code}, and a non-empty token
// protects that route.
func (h *Handler) UseStats(rec StatsRecorder, reader StatsReader, token string) {
	if rec != nil {
		h.stats = rec
	}
	h.statsReader = reader
	h.statsToken = token
}

// Request and response bodies. The `json:"..."` tags set the JSON key names.
type shortenResponse struct {
	ShortURL string `json:"short_url"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type shortenRequest struct {
	URL   string `json:"url"`
	Alias string `json:"alias"` // optional; empty means "generate a code for me"
}

// Middleware wraps a handler, for example with a rate limiter.
type Middleware func(http.Handler) http.Handler

func noMiddleware(next http.Handler) http.Handler { return next }

// writeJSON sends v as JSON with the given status code.
// Order matters: headers must be set BEFORE WriteHeader, because changes
// made after WriteHeader are ignored.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// Shorten handles POST /shorten: read JSON, validate, store, respond.
func (h *Handler) Shorten(w http.ResponseWriter, r *http.Request) {
	// Cap the body at 1 MB so a client can't make us read unlimited data.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "body must be JSON like {\"url\": \"https://...\"}"})
		return
	}

	cleaned, err := ValidateURL(req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	// Validate the alias (if any) BEFORE touching the store, same as the URL.
	if req.Alias != "" {
		if err := ValidateAlias(req.Alias); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
	}

	// Two paths to a Link: generated code, or caller-chosen code.
	var link store.Link
	if req.Alias == "" {
		link, err = h.store.Create(r.Context(), cleaned)
	} else {
		link, err = h.store.CreateWithCode(r.Context(), cleaned, req.Alias)
	}

	// A taken alias is the client's conflict, not our failure: 409.
	if errors.Is(err, store.ErrAliasTaken) {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "alias already taken"})
		return
	}
	if err != nil {
		// Log the details for us; send the client a generic message.
		log.Printf("create link: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	}

	writeJSON(w, http.StatusCreated, shortenResponse{ShortURL: h.baseURL + "/" + link.Code})
}

// Redirect handles GET /{code}: resolve, record the click, redirect.
func (h *Handler) Redirect(w http.ResponseWriter, r *http.Request) {
	// PathValue reads the {code} wildcard from the route pattern (Go 1.22+).
	code := r.PathValue("code")

	longURL, err := h.store.Resolve(r.Context(), code)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "link not found"})
		return
	}
	if err != nil {
		log.Printf("resolve %q: %v", code, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	}

	// An in-memory increment: no database, no network, no error to handle.
	// Only successful lookups are counted, so 404s never reach the counter.
	h.clicks.Record(code)

	// Analytics, aggregated in memory like the counter: no database here.
	h.stats.Record(code, time.Now(), r.Referer(), r.UserAgent())

	// 302 is not cached by browsers, so every click reaches us and is counted.
	http.Redirect(w, r, longURL, http.StatusFound)
}

// Routes returns the router with every endpoint registered. main uses it to
// serve traffic and tests use it to send requests, so both exercise the
// same routing. Each route is wrapped in its own (possibly empty) limiter.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /shorten", h.shortenMW(http.HandlerFunc(h.Shorten)))
	mux.Handle("GET /{code}", h.redirectMW(http.HandlerFunc(h.Redirect)))
	if h.statsReader != nil {
		mux.HandleFunc("GET /stats/{code}", h.Stats)
	}
	return mux
}

// UseLimits sets per-route middleware. A nil argument leaves that route
// unlimited, so creating links and redirecting can have different limits.
func (h *Handler) UseLimits(shorten, redirect Middleware) {
	if shorten != nil {
		h.shortenMW = shorten
	}
	if redirect != nil {
		h.redirectMW = redirect
	}
}

// Stats handles GET /stats/{code}?hours=24.
func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	if h.statsToken != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Constant-time compare so response timing can't reveal the token.
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.statsToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
	}

	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 720 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "hours must be a number from 1 to 720"})
			return
		}
		hours = n
	}
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Truncate(time.Hour)

	code := r.PathValue("code")
	st, err := h.statsReader.Stats(r.Context(), code, since)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "link not found"})
		return
	}
	if err != nil {
		log.Printf("stats %q: %v", code, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
