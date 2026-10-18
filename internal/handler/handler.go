package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"urlshortener/internal/store"
)

// Handler holds what the HTTP handlers need. It depends on the Store
// interface, never on Postgres, so the database could be swapped or faked.
type Handler struct {
	store   store.Store
	baseURL string // public address of this server, e.g. "http://localhost:8080"
}

// NewHandler is the constructor. TrimRight removes a trailing "/" so we
// never build URLs like "http://host//abc".
func NewHandler(s store.Store, baseURL string) *Handler {
	return &Handler{store: s, baseURL: strings.TrimRight(baseURL, "/")}
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

// Redirect handles GET /{code}: look up, count the click, redirect.
func (h *Handler) Redirect(w http.ResponseWriter, r *http.Request) {
	// PathValue reads the {code} wildcard from the route pattern (Go 1.22+).
	code := r.PathValue("code")

	link, err := h.store.Get(r.Context(), code)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "link not found"})
		return
	}
	if err != nil {
		log.Printf("get link %q: %v", code, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	}

	// If counting fails, log it but still redirect: the visitor shouldn't
	// lose their page because our statistics broke.
	if err := h.store.IncrementClicks(r.Context(), code); err != nil {
		log.Printf("increment clicks %q: %v", code, err)
	}

	// 302 (Found) is not cached by browsers, so every click reaches us and
	// gets counted. A 301 would let browsers skip the server on repeat visits.
	http.Redirect(w, r, link.LongURL, http.StatusFound)
}

// Routes returns the router with every endpoint registered. main uses it to
// serve traffic and tests use it to send requests, so both exercise the
// same routing. (r.PathValue only works for requests that went through
// the mux, which is why tests must not call h.Redirect directly.)
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /shorten", h.Shorten)
	mux.HandleFunc("GET /{code}", h.Redirect)
	return mux
}
