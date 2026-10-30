package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"urlshortener/internal/clicks"
	"urlshortener/internal/handler"
	"urlshortener/internal/idgen"
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

// splitCSV splits "a, b,,c" into ["a" "b" "c"].
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// envReader parses numeric env vars and remembers the first error.
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
// off (rate <= 0) or Redis isn't configured (the state must be shared).
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

func main() {
	// "/server -healthcheck" is how Docker checks the container: the image
	// has no shell or curl, so the binary asks its own /healthz.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// healthcheck returns the process exit code: 0 healthy, 1 not.
func healthcheck() int {
	_, port, err := net.SplitHostPort(getenv("ADDR", ":8080"))
	if err != nil {
		return 1
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// withInstance labels every response with the container that served it.
func withInstance(next http.Handler) http.Handler {
	host, _ := os.Hostname()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", host)
		next.ServeHTTP(w, r)
	})
}

func run() error {
	// ctx is cancelled on Ctrl+C (SIGINT) or SIGTERM (Docker, Kubernetes).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- configuration ----
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
	nodeID := cfg.int("NODE_ID", -1)                        // -1 = lease one from Redis
	if cfg.err != nil {
		return cfg.err
	}

	trusted, err := ratelimit.ParseTrustedProxies(getenv("TRUSTED_PROXIES", ""))
	if err != nil {
		return fmt.Errorf("parse TRUSTED_PROXIES: %w", err)
	}

	// ---- Postgres: one pool per shard ----
	// DATABASE_URLS lists the shards; the ORDER defines the shard numbers, so
	// never reorder it. Without it, DATABASE_URL is the only shard.
	dbURLs := splitCSV(getenv("DATABASE_URLS", ""))
	if len(dbURLs) == 0 {
		dbURLs = []string{dbURL}
	}
	var pools []*pgxpool.Pool
	var pgs []*store.PostgresStore
	for i, u := range dbURLs {
		p, err := pgxpool.New(ctx, u)
		if err != nil {
			return fmt.Errorf("shard %d: create pool: %w", i, err)
		}
		defer p.Close()
		if err := p.Ping(ctx); err != nil {
			return fmt.Errorf("shard %d: ping database: %w", i, err)
		}
		pools = append(pools, p)
		pgs = append(pgs, store.NewPostgresStore(p))
	}
	log.Printf("connected to %d Postgres shard(s)", len(pools))

	// ---- Redis (optional): cache, rate limiter, node leases ----
	var rdb *redis.Client
	if redisAddr != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr:       redisAddr,
			MaxRetries: -1, // -1 disables retries: a cache should fail fast
		})
		defer rdb.Close()

		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			log.Printf("warning: redis unreachable (%v); continuing", err)
		}
		cancel()
	} else {
		log.Println("REDIS_ADDR not set; running without a cache")
	}

	// ---- ID generator: every running instance needs its OWN node number ----
	var leaseLost <-chan struct{} // nil (never fires) unless we hold a lease
	switch {
	case nodeID >= 0:
		log.Printf("id generator: node %d (fixed by NODE_ID)", nodeID)
	case rdb != nil:
		host, _ := os.Hostname()
		lease, n, err := idgen.ClaimNode(ctx, rdb, fmt.Sprintf("%s-%d", host, os.Getpid()))
		if err != nil {
			return fmt.Errorf("claim node id: %w", err)
		}
		defer func() { // runs after the counters below are closed (LIFO)
			relCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := lease.Release(relCtx); err != nil {
				log.Printf("release node lease: %v", err)
			}
		}()
		nodeID, leaseLost = int(n), lease.Lost()
		log.Printf("id generator: node %d (leased from Redis)", nodeID)
	default:
		nodeID = 0
		log.Println("warning: no NODE_ID and no Redis: node 0 is only safe with ONE instance")
	}
	gen, err := idgen.New(int64(nodeID))
	if err != nil {
		return fmt.Errorf("id generator: %w", err)
	}

	// ---- store stack: shards, then the cache on top ----
	sharded := store.NewShardedStore(pgs, gen)
	var s store.Store = sharded
	if rdb != nil {
		s = store.NewCachedStore(s, rdb)
		log.Printf("redis cache enabled (%s)", redisAddr)
	}

	// Clicks are counted in memory and flushed in batches. These deferred
	// Closes run on EVERY return path, after srv.Shutdown has finished, and
	// before the pools and Redis are closed (deferred calls run LIFO).
	counter := clicks.NewCounter(s, flushEvery)
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := counter.Close(flushCtx); err != nil {
			log.Printf("flush clicks on shutdown: %v", err)
			return
		}
		log.Println("click counter flushed")
	}()

	statsCounter := clicks.NewStatsCounter(sharded, flushEvery)
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := statsCounter.Close(flushCtx); err != nil {
			log.Printf("flush click stats on shutdown: %v", err)
			return
		}
		log.Println("click stats flushed")
	}()

	h := handler.NewHandler(s, counter, baseURL)
	h.UseStats(statsCounter, sharded, getenv("STATS_TOKEN", ""))
	h.UseReadiness(func(ctx context.Context) error { // ready only if EVERY shard answers
		for i, p := range pools {
			if err := p.Ping(ctx); err != nil {
				return fmt.Errorf("shard %d: %w", i, err)
			}
		}
		return nil
	})

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
		Addr:              addr,
		Handler:           withInstance(handler.LogRequests(h.Routes())),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		errCh <- srv.ListenAndServe()
	}()

	var exitErr error
	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-leaseLost:
		// Another instance may now hold our node number: stop issuing IDs.
		// Shut down cleanly and exit with an error so Docker restarts us
		// with a fresh lease.
		exitErr = errors.New("node id lease lost")
		log.Println("shutting down: node id lease lost...")
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
	return exitErr
}
