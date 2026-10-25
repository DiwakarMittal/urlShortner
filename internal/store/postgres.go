package store

import (
	"context"
	"errors"
	"fmt"
	"urlshortener/internal/shortcode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore implements Store using a pgx connection pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// Compile-time check: this line fails to build if PostgresStore ever
// stops satisfying the Store interface.
var _ Store = (*PostgresStore)(nil)

// NewPostgresStore is a constructor: Go has no constructors, so by
// convention we write a New... function.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Create saves a long URL and returns the new Link, including its code.
// The code is derived from the row's ID, which only exists after the INSERT,
// so we need two statements. They run in one transaction so they succeed or
// fail together (no row left behind without a code).
func (s *PostgresStore) Create(ctx context.Context, longURL string) (Link, error) {
	link := Link{LongURL: longURL}

	// BeginFunc starts a transaction, runs our function, commits if it
	// returns nil, and rolls back if it returns an error.
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		query := `INSERT INTO links (long_url) VALUES ($1) RETURNING id, created_at`
		// Step 1: insert and get the generated id and timestamp back.
		// $1 is a placeholder; the value is sent separately from the SQL,
		// which prevents SQL injection.
		err := tx.QueryRow(ctx, query, longURL).Scan(&link.ID, &link.CreatedAt) // Scan writes into our fields via pointers
		if err != nil {
			return fmt.Errorf("insert link: %w", err)
		}

		// Step 2: turn the id into a short code.
		link.Code = shortcode.Encode(link.ID)

		// Step 3: save the code on the same row.
		_, err = tx.Exec(ctx,
			`UPDATE links SET code = $1 WHERE id = $2`,
			link.Code, link.ID,
		)
		if err != nil {
			return fmt.Errorf("set code: %w", err)
		}
		return nil
	})
	if err != nil {
		return Link{}, err
	}
	return link, nil
}

// Get finds a link by its short code, or returns ErrNotFound.
func (s *PostgresStore) Get(ctx context.Context, code string) (Link, error) {
	var l Link

	err := s.pool.QueryRow(ctx,
		`SELECT id, code, long_url, clicks, created_at FROM links WHERE code = $1`,
		code,
	).Scan(&l.ID, &l.Code, &l.LongURL, &l.Clicks, &l.CreatedAt)

	// pgx reports "zero rows" as pgx.ErrNoRows. We translate it to our own
	// ErrNotFound so handlers never need to know about pgx.
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("get link: %w", err)
	}
	return l, nil
}

// IncrementClicks adds one to the click count. The increment happens inside
// Postgres in a single statement, so concurrent visits can't lose counts.
func (s *PostgresStore) IncrementClicks(ctx context.Context, code string) error {
	// Exec is for statements that return no rows.
	tag, err := s.pool.Exec(ctx,
		`UPDATE links SET clicks = clicks + 1 WHERE code = $1`,
		code,
	)
	if err != nil {
		return fmt.Errorf("increment clicks: %w", err)
	}

	// RowsAffected is 0 when no row matched, meaning the code doesn't exist.
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateWithCode inserts a link under a caller-chosen code. It is ONE
// statement, so no transaction is needed (unlike Create, which needs the
// generated ID before it can compute the code).
//
// Uniqueness is enforced by the UNIQUE constraint on code, not by a SELECT
// first: check-then-insert is a race, because two requests could both see
// "not taken" and both insert. The database is the single source of truth;
// we just translate its error.
func (s *PostgresStore) CreateWithCode(ctx context.Context, longURL, code string) (Link, error) {
	link := Link{LongURL: longURL, Code: code}

	err := s.pool.QueryRow(ctx,
		`INSERT INTO links (long_url, code) VALUES ($1, $2) RETURNING id, created_at`,
		longURL, code,
	).Scan(&link.ID, &link.CreatedAt)
	if err != nil {
		// errors.As looks through the wrapping chain for a *pgconn.PgError.
		// 23505 is Postgres' "unique_violation"; checking the constraint
		// name makes sure it's the code column and nothing else.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "links_code_key" {
			return Link{}, ErrAliasTaken
		}
		return Link{}, fmt.Errorf("insert link with code: %w", err)
	}
	return link, nil
}

// Resolve fetches just the long URL, so the redirect path reads one
// column instead of the whole row.
func (s *PostgresStore) Resolve(ctx context.Context, code string) (string, error) {
	var longURL string

	err := s.pool.QueryRow(ctx,
		`SELECT long_url FROM links WHERE code = $1`,
		code,
	).Scan(&longURL)

	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve link: %w", err)
	}
	return longURL, nil
}
