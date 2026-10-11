package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"
	"urlshortener/internal/handler"
	"urlshortener/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

// getenv returns the environment variable, or fallback when it's empty.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

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

	// Configuration comes from the environment, with local defaults.
	dbURL := getenv("DATABASE_URL", "postgres://shortener:shortener@localhost:5433/shortener")
	baseURL := getenv("BASE_URL", "http://localhost:8080") // used to build short links
	addr := getenv("ADDR", ":8080")                        // where the server listens

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
	h := handler.NewHandler(s, baseURL)

	// Routes: Go 1.22+ patterns include the method and path wildcards.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /shorten", h.Shorten)
	mux.HandleFunc("GET /{code}", h.Redirect)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
