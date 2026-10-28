package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"urlshortener/internal/clicks"
	"urlshortener/internal/handler"
	"urlshortener/internal/ratelimit"
	"urlshortener/internal/store"
)

// getenv returns the environment variable, or fallback when it's empty.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envReader parses numeric env vars and remembers the first error, so the
// caller can read several values and check once.
type envReader struct{ err error }

func (e *envReader) float(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil && e.err == nil {
		e.err = fmt.Errorf("parse %s: %w", key, err)
	}
	return f
}

func (e *envReader) int(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil && e.err == nil {
		e.err = fmt.Errorf("parse %s: %w", key, err)
	}
	return n
}

// buildLimit returns the middleware for one route, or nil when the limit is
// switched off (rate <= 0) or Redis isn't configured: the limiter's state
// has to live in Redis to be shared between instances.
func buildLimit(rdb *redis.Client, name string, ratePerSec float64, burst int, key func(*http.Request) string) (handler.Middleware, error) {
	if ratePerSec <= 0 {
		log.Printf("rate limit %q: off", name)
		return nil, nil
	}
	if rdb == nil {
		log.Printf("warning: rate limit %q needs REDIS_ADDR; disabled", name)
		return nil, nil
	}
	if burst < 1 {
		burst = int(math.Ceil(ratePerSec)) // default: one second's worth
	}

	l, err := ratelimit.New(rdb, ratelimit.Rule{Name: name, Rate: ratePerSec, Burst: burst})
	if err != nil {
		return nil, err
	}
	log.Printf("rate limit %q: %.4g/s, burst %d", name, ratePerSec, burst)
	return ratelimit.Middleware(l, key, burst), nil
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

	flushEvery, err := time.ParseDuration(getenv("CLICK_FLUSH_INTERVAL", "1s"))
	if err != nil {
		return fmt.Errorf("parse CLICK_FLUSH_INTERVAL: %w", err)
	}

	cfg := &envReader{}
	shortenPerMin := cfg.float("RATE_SHORTEN_PER_MIN", 10) // 0 = off
	shortenBurst := cfg.int("RATE_SHORTEN_BURST", 10)
	redirectPerSec := cfg.float("RATE_REDIRECT_PER_SEC", 0) // 0 = off (the default)
	redirectBurst := cfg.int("RATE_REDIRECT_BURST", 0)      // 0 = one second's worth
	if cfg.err != nil {
		return cfg.err
	}

	trusted, err := ratelimit.ParseTrustedProxies(getenv("TRUSTED_PROXIES", ""))
	if err != nil {
		return fmt.Errorf("parse TRUSTED_PROXIES: %w", err)
	}

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

	// rdb stays nil when Redis isn't configured; the cache and the rate
	// limiter both use it when present.
	var rdb *redis.Client
	if redisAddr != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr:       redisAddr,
			MaxRetries: -1, // -1 disables retries: a cache should fail fast
		})
		defer rdb.Close()

		// Redis is optional: if it's down at startup, warn and carry on.
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

	// Clicks are counted in memory and flushed in batches. The deferred
	// Close runs on EVERY return path, always AFTER srv.Shutdown has
	// finished (we only return after it), so no request can record a click
	// after the final flush. Deferred calls run last-in-first-out, so this
	// runs before pool.Close and rdb.Close, while both are still open.
	counter := clicks.NewCounter(s, flushEvery)
	defer func() {
		// The main ctx is already cancelled by now, so use a fresh one.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := counter.Close(flushCtx); err != nil {
			log.Printf("flush clicks on shutdown: %v", err)
			return
		}
		log.Println("click counter flushed")
	}()

	h := handler.NewHandler(s, counter, baseURL)

	// Rate limits: who is "the client" is decided by ClientIP, which only
	// believes X-Forwarded-For when the connection comes from a trusted proxy.
	clientKey := func(r *http.Request) string { return ratelimit.ClientIP(r, trusted) }

	shortenMW, err := buildLimit(rdb, "shorten", shortenPerMin/60, shortenBurst, clientKey)
	if err != nil {
		return fmt.Errorf("shorten rate limit: %w", err)
	}
	redirectMW, err := buildLimit(rdb, "redirect", redirectPerSec, redirectBurst, clientKey)
	if err != nil {
		return fmt.Errorf("redirect rate limit: %w", err)
	}
	h.UseLimits(shortenMW, redirectMW)

	srv := &http.Server{
		Addr: addr,
		// LogRequests is outermost, so 429 responses are logged too.
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
