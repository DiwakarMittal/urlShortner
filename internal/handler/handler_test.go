package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"urlshortener/internal/shortcode"
	"urlshortener/internal/store"
)

// ---------------------------------------------------------------------------
// fakeStore: an in-memory store.Store for tests. No database, no Docker.
// ---------------------------------------------------------------------------

type fakeStore struct {
	links map[string]store.Link // code -> link
	err   error                 // when set, every method fails with it (simulates a DB outage)
}

// Compile-time check: the build fails if the fake drifts from the interface.
var _ store.Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{links: make(map[string]store.Link)}
}

func (f *fakeStore) Create(ctx context.Context, longURL string) (store.Link, error) {
	if f.err != nil {
		return store.Link{}, f.err
	}
	// Mimic Postgres: IDs count up from 1, and the code is derived from the ID.
	id := int64(len(f.links) + 1)
	l := store.Link{ID: id, Code: shortcode.Encode(id), LongURL: longURL}
	f.links[l.Code] = l
	return l, nil
}

func (f *fakeStore) Get(ctx context.Context, code string) (store.Link, error) {
	if f.err != nil {
		return store.Link{}, f.err
	}
	l, ok := f.links[code]
	if !ok {
		// Must be the same sentinel the real store returns, because the
		// handler checks it with errors.Is to choose 404.
		return store.Link{}, store.ErrNotFound
	}
	return l, nil
}

func (f *fakeStore) IncrementClicks(ctx context.Context, code string) error {
	if f.err != nil {
		return f.err
	}
	l, ok := f.links[code]
	if !ok {
		return store.ErrNotFound
	}
	// A map holds COPIES of structs, so f.links[code].Clicks++ won't compile.
	// Read the copy, change it, write it back.
	l.Clicks++
	f.links[code] = l
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestHandler gives each test a fresh fake, so tests never share state.
func newTestHandler() (*Handler, *fakeStore) {
	f := newFakeStore()
	return NewHandler(f, "http://short.test"), f
}

// do sends one request through the real router and returns the recorded
// response. No server starts and no port is used: it's a plain function call.
func do(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// POST /shorten
// ---------------------------------------------------------------------------

func TestShorten_OK(t *testing.T) {
	h, f := newTestHandler()

	rec := do(h, "POST", "/shorten", `{"url":"https://example.com"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// Decode into our response struct instead of comparing raw strings.
	var resp shortenResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if want := "http://short.test/1"; resp.ShortURL != want {
		t.Errorf("short_url = %q, want %q", resp.ShortURL, want)
	}

	// The link must really be in the store.
	got, ok := f.links["1"]
	if !ok || got.LongURL != "https://example.com" {
		t.Errorf("store has %+v, want a link with code 1 for https://example.com", got)
	}
}

func TestShorten_StoresCleanedURL(t *testing.T) {
	h, f := newTestHandler()

	rec := do(h, "POST", "/shorten", `{"url":"   https://example.com   "}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	// We must store the trimmed URL that ValidateURL returned, not the raw input.
	if got := f.links["1"].LongURL; got != "https://example.com" {
		t.Errorf("stored %q, want %q", got, "https://example.com")
	}
}

func TestShorten_BadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"javascript scheme", `{"url":"javascript:alert(1)"}`},
		{"not json", `not json`},
		{"empty url", `{"url":""}`},
		{"missing field", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, f := newTestHandler()

			rec := do(h, "POST", "/shorten", tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			// Bad input must never reach the store.
			if len(f.links) != 0 {
				t.Errorf("store has %d links, want 0", len(f.links))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GET /{code}
// ---------------------------------------------------------------------------

func TestRedirect_OK(t *testing.T) {
	h, f := newTestHandler()
	f.links["abc"] = store.Link{ID: 1, Code: "abc", LongURL: "https://example.com/page"}

	rec := do(h, "GET", "/abc", "")

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "https://example.com/page" {
		t.Errorf("Location = %q, want %q", got, "https://example.com/page")
	}
	if got := f.links["abc"].Clicks; got != 1 {
		t.Errorf("Clicks = %d, want 1", got)
	}
}

func TestRedirect_CountsEveryVisit(t *testing.T) {
	h, f := newTestHandler()
	f.links["abc"] = store.Link{ID: 1, Code: "abc", LongURL: "https://example.com"}

	do(h, "GET", "/abc", "")
	do(h, "GET", "/abc", "")

	if got := f.links["abc"].Clicks; got != 2 {
		t.Errorf("Clicks = %d, want 2", got)
	}
}

func TestRedirect_NotFound(t *testing.T) {
	h, _ := newTestHandler()

	rec := do(h, "GET", "/nope", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// ---------------------------------------------------------------------------
// Store failures: impossible to trigger by hand, easy with a fake.
// ---------------------------------------------------------------------------

func TestStoreFailure_Returns500WithoutLeaking(t *testing.T) {
	const secret = "db exploded: password=hunter2"

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"shorten", "POST", "/shorten", `{"url":"https://example.com"}`},
		{"redirect", "GET", "/abc", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, f := newTestHandler()
			f.err = errors.New(secret) // every store call now fails

			rec := do(h, tt.method, tt.path, tt.body)

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
			}
			// The client must get a generic message, never internals.
			if strings.Contains(rec.Body.String(), "db exploded") {
				t.Errorf("response leaked the internal error: %s", rec.Body.String())
			}
		})
	}
}
