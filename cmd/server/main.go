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
	"github.com/redis/go-redis/v9"

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
// because log.Fatal calls os.Exit, which skips deferred calls.
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// ctx is cancelled on Ctrl+C (SIGINT) or SIGTERM (Docker, Kubernetes).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Configuration comes from the environment, with local defaults.
	dbURL := getenv("DATABASE_URL", "postgres://shortener:shortener@localhost:5433/shortener")
	baseURL := getenv("BASE_URL", "http://localhost:8080")
	addr := getenv("ADDR", ":8080")
	redisAddr := getenv("REDIS_ADDR", "") // empty = run without a cache

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	log.Println("connected to Postgres")

	// Build the store stack. Each layer satisfies store.Store, so the
	// handlers never know whether a cache is present.
	var s store.Store = store.NewPostgresStore(pool)

	if redisAddr != "" {
		rdb := redis.NewClient(&redis.Options{
			Addr:       redisAddr,
			MaxRetries: -1, // -1 disables retries: a cache should fail fast
		})
		defer rdb.Close()

		// The cache is optional: if Redis is down at startup, warn and
		// carry on. Requests fall back to Postgres.
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			log.Printf("warning: redis unreachable (%v); continuing without a warm cache", err)
		}
		cancel()

		s = store.NewCachedStore(s, rdb)
		log.Printf("redis cache enabled (%s)", redisAddr)
	} else {
		log.Println("REDIS_ADDR not set; running without a cache")
	}

	h := handler.NewHandler(s, baseURL)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler.LogRequests(h.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		stop() // a second Ctrl+C now force-kills
		log.Println("shutting down...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server error: %w", err)
	}

	log.Println("server stopped cleanly")
	return nil
}
