package ratelimit

import (
	"log"
	"math"
	"net/http"
	"strconv"
)

// Middleware limits the handlers it wraps. clientKey says who the caller
// is (see ClientIP); limit is only used for the X-RateLimit-Limit header.
//
// If the limiter itself fails (Redis down or slow), requests are ALLOWED:
// "fail open". A limiter outage should not take the whole service down.
// The cost is that abuse protection is off while Redis is unavailable.
func Middleware(l Allower, clientKey func(*http.Request) string, limit int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, err := l.Allow(r.Context(), clientKey(r))
			if err != nil {
				log.Printf("rate limiter unavailable, allowing request: %v", err)
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))

			if !d.Allowed {
				// Retry-After is whole seconds; round UP so a client that
				// obeys it is never rejected again for waiting too little.
				secs := int(math.Ceil(d.RetryAfter.Seconds()))
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"rate limit exceeded"}` + "\n"))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
