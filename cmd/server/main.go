package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"urlshortener/internal/handler"
	"urlshortener/internal/store"
)

// getenv returns the environment variable, or fallback when it's empty.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()

	// Configuration comes from the environment, with local defaults.
	dbURL := getenv("DATABASE_URL", "postgres://shortener:shortener@localhost:5433/shortener")
	baseURL := getenv("BASE_URL", "http://localhost:8080") // used to build short links
	addr := getenv("ADDR", ":8080")                        // where the server listens

	// One shared connection pool for the whole app.
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// New() may not connect immediately; Ping proves the DB is reachable.
	if err = pool.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	log.Println("connected to Postgres")

	// Wire the layers: pool -> store -> handler. The handler only sees
	// the store.Store interface.
	var s store.Store = store.NewPostgresStore(pool)
	h := handler.NewHandler(s, baseURL)

	// Explicit Server so we can set timeouts; without them, slow clients
	// can hold connections open forever.
	srv := &http.Server{
		Addr:              addr,
		Handler:           h.Routes(), // routes now live in the handler package
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
