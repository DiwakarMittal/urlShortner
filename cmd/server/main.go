package main

import (
	"context"
	"errors"
	"log"
	"os"
	"urlshortener/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 1. Create a context
// 2. Read DATABASE_URL from the environment.
//    If it's empty, use the default connection string.
// 3. Create the pool with pgxpool.New(...)
//    If err != nil, log.Fatal(err)
// 4. defer pool.Close()
// 5. Ping the pool.
//    If err != nil, log.Fatal(err)
// 6. Print "connected to postgres"

func main() {
	// A context carries deadlines/cancellation; Background is the empty root.
	ctx := context.Background()

	// Read config from the environment; fall back to the local Docker DB.
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://shortener:shortener@localhost:5433/shortener"
	}

	// One shared connection pool for the whole app.
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close() // release connections on exit

	// New() may not connect immediately; Ping proves the DB is reachable.
	if err = pool.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	log.Println("connected to Postgres")

	// Build the store. We keep the variable typed as the store interface.
	// the same way the handler will see it later

	var s store.Store = store.NewPostgresStore(pool)

	// 1. create a link and print the same
	link, err := s.Create(ctx, "https://example.com/some/long/path")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("created: %+v", link)

	// 2. Look it up by its code.
	got, err := s.Get(ctx, link.Code)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fetched: %+v", got)

	// 3. Count one click, then fetch again; Clicks should now be 1 higher.
	if err = s.IncrementClicks(ctx, link.Code); err != nil {
		log.Fatal(err)
	}
	got, err = s.Get(ctx, link.Code)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("after click: clicks=%d", got.Clicks)

	// 4. A code that doesn't exist must give ErrNotFound.
	// errors.Is checks the error even if it was wrapped.
	_, err = s.Get(ctx, "nope")
	if errors.Is(err, store.ErrNotFound) {
		log.Println("missing code correctly returned ErrNotFound")
	} else {
		log.Fatalf("expected ErrNotFound, got: %v", err)
	}
}
