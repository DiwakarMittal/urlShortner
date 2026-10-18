package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

// main only reports the final error. All the real work lives in run(),
// because log.Fatal calls os.Exit, which skips deferred calls. Since run()
// has already returned by the time log.Fatal executes, its defers
// (like pool.Close) have run.
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// ctx is cancelled when the process receives Ctrl+C (SIGINT) or SIGTERM
	// (what Docker, Kubernetes, and systemd send to stop an app).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Configuration comes from the environment, with local defaults.
	dbURL := getenv("DATABASE_URL", "postgres://shortener:shortener@localhost:5433/shortener")
	baseURL := getenv("BASE_URL", "http://localhost:8080") // used to build short links
	addr := getenv("ADDR", ":8080")                        // where the server listens

	// One shared connection pool for the whole app. Errors are returned
	// (wrapped with context) instead of calling log.Fatal, so defers run.
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close() // now runs on every exit path

	// New() may not connect immediately; Ping proves the DB is reachable.
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
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

	// ListenAndServe blocks, so run it in a goroutine and report its result
	// on a channel. The buffer of 1 means the goroutine can always finish
	// its send, even if nobody is receiving anymore (e.g. after a signal).
	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		errCh <- srv.ListenAndServe()
	}()

	// Wait for whichever happens first: the server fails on its own
	// (for example, the port is already in use) or we are told to stop.
	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		// Restore default signal behavior so a SECOND Ctrl+C kills the
		// process immediately if shutdown ever hangs.
		stop()
		log.Println("shutting down...")
	}

	// Give in-flight requests up to 10 seconds to finish. This needs a
	// fresh context: ctx is already cancelled, and Shutdown would give up
	// immediately if we passed it in.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown stops accepting new connections, then waits for active
	// requests. After it is called, ListenAndServe returns
	// http.ErrServerClosed into errCh; nobody reads it, and that's fine.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Optional: confirm the server goroutine ended the expected way.
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server error: %w", err)
	}

	log.Println("server stopped cleanly")
	return nil
}
