package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when no link exists for the given code.
// Handlers check it with errors.Is(err, store.ErrNotFound) to send a 404.
var ErrNotFound = errors.New("link not found")

// Link mirrors one row of the "links" table.
type Link struct {
	ID        int64     // database primary key; the short code is derived from it
	Code      string    // short code like "aB3xYz" (not the full URL)
	LongURL   string    // the original URL we redirect to
	Clicks    int64     // number of times the short link was visited
	CreatedAt time.Time // set by Postgres via DEFAULT now()
}

// Store describes what the app needs from storage. Handlers depend on this
// interface, so the database can be swapped or faked in tests.
type Store interface {
	// Create saves a long URL and returns the new Link (with ID and Code).
	Create(ctx context.Context, longURL string) (Link, error)
	// Get finds a link by its short code. Returns ErrNotFound if missing.
	Get(ctx context.Context, code string) (Link, error)
	// IncrementClicks adds one to the click count for the code.
	IncrementClicks(ctx context.Context, code string) error
}
