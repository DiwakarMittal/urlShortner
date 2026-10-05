package main

import (
	"context"
	"log"
	"os"

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
}
