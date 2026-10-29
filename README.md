# URL Shortener (Go + Postgres + Redis)

A learning project that grows from a basic shortener into a read-heavy,
horizontally scalable service: validation, custom aliases, a Redis cache,
batched click counting, graceful shutdown, tests at four levels, and
measured benchmarks for every performance change.

## How it works

1. **Create**: client sends a long URL, server stores it, returns a short code.
2. **Redirect**: visitor opens `/<code>`, server looks it up (Redis first,
   Postgres on a miss) and replies with a redirect (`302` + `Location`
   header) to the long URL. The click is counted in memory and flushed to
   Postgres in batches.

The short link is only a pointer. The original page is unchanged, and the
shortener server must be publicly reachable for others to use the links.

## API

| Request | Success | Errors |
|---|---|---|
| `POST /shorten` with `{"url": "...", "alias": "optional-name"}` | `201` `{"short_url": "..."}` | `400` bad JSON / URL / alias, `409` alias taken, `500` |
| `GET /{code}` | `302` redirect to the long URL (click counted) | `404` unknown code, `500` |

## Quick start

Everything (app, Postgres, Redis) runs with Docker Compose:

```
docker compose up -d --build
docker compose exec -T db psql -U shortener -d shortener < migrations/001_create_links.sql   # first time only
```

Try it:

```
curl -i -X POST localhost:8080/shorten -d '{"url":"https://example.com"}'
curl -i localhost:8080/<code>
```

Custom alias (3 to 32 characters, must contain a `-` or `_`):

```
curl -i -X POST localhost:8080/shorten -d '{"url":"https://example.com","alias":"my-link"}'
```

Logs: `docker compose logs -f app`. Stop gracefully: `docker compose stop app`.

Run all tests (with the race detector): `go test -race -count=1 ./...`

Integration tests (real Postgres; see Step 14 for the one-time setup):

```
TEST_DATABASE_URL="postgres://shortener:shortener@localhost:5433/shortener_test" go test -count=1 -v ./internal/store/
```

Port 8080 is used by the container. Don't also run `go run ./cmd/server`
at the same time (you would get "address already in use").

| Env var | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://shortener:shortener@localhost:5433/shortener` | Postgres connection (Compose sets `db:5432`) |
| `REDIS_ADDR` | empty | Redis address; empty runs without a cache (Compose sets `redis:6379`) |
| `BASE_URL` | `http://localhost:8080` | public address used to build short links |
| `ADDR` | `:8080` | listen address |
| `CLICK_FLUSH_INTERVAL` | `1s` | how often batched click counts are written to Postgres |

## Tech choices (and why)

| Choice | Why |
|---|---|
| Go `net/http` | Standard library is enough; Go 1.22+ `ServeMux` supports `GET /{code}` patterns, so no router needed |
| Postgres | Production-grade; `BIGSERIAL` gives unique IDs and `UPDATE ... clicks + n` is atomic |
| `pgx` + `pgxpool` | Modern Postgres driver; the pool reuses connections instead of opening one per request |
| Base62 of the row ID | IDs are unique by construction, so no collision checks or retries |
| `Store` interface | Handlers depend on an interface, not Postgres: simple handlers, swappable DB, fakes in tests, and a cache that slots in as a decorator |
| Redis cache | Redirects are read-heavy; hot links are served without touching Postgres |
| In-memory click counter | Keeps the click write off the request path (see Benchmarks) |
| Docker multi-stage build | About 20 MB final image, no shell, non-root; the prerequisite for running several instances |

## Project structure

```
cmd/server/main.go                 entry point: config, wiring, graceful shutdown
internal/store/store.go            Link struct, Store interface, errors
internal/store/postgres.go         Postgres implementation
internal/store/postgres_test.go    integration tests (need TEST_DATABASE_URL)
internal/store/cached.go           Redis cache decorator (cache-aside, negative caching)
internal/store/cached_test.go      cache tests (miniredis, no Docker needed)
internal/clicks/counter.go         in-memory click aggregation + batched flush
internal/clicks/counter_test.go    counter tests (race-detector friendly)
internal/handler/handler.go        HTTP handlers and routes
internal/handler/validate.go       URL and alias validation
internal/handler/middleware.go     access log middleware
internal/handler/*_test.go         handler, validation, middleware tests (fake store)
internal/shortcode/base62.go       base62 encoding (+ test)
migrations/001_create_links.sql    schema
Dockerfile                         multi-stage build of the app
docker-compose.yaml                app + Postgres + Redis
```

`internal/` is enforced by Go: other modules cannot import these packages.

## Inspecting the database (TablePlus)

- Connection: PostgreSQL, host `127.0.0.1`, port **5433** (the host side
  of the Docker port mapping), user/password/database `shortener`.
- Browse rows under `public` -> `links` (Data tab) and the schema in the
  Structure tab. Refresh (Cmd+R) to see new clicks; it doesn't live-update,
  and click counts lag by up to the flush interval.
- Manual edits and `UPDATE`/`DELETE` here affect real data; always use a
  `WHERE`.

## Benchmarks

Tool: `hey -n 20000 -c 50 -disable-redirects http://localhost:8080/1`
(the scheme is required, and `-disable-redirects` stops `hey` from
following the redirect to the target site). Everything runs on one laptop
inside Docker's VM, so only the *relative* numbers matter. Short runs vary
by about 8%; use `hey -z 10s` and the median of three for comparisons.

| Step | Requests/sec | p50 | p99 | Notes |
|---|---|---|---|---|
| 15: Postgres only | 4,398 | 10.6 ms | 20.9 ms | SELECT + UPDATE per redirect |
| 16: Redis cache, `/1` | 4,426 | 10.6 ms | 19.4 ms | hit rate 99.997%, but throughput unchanged |
| 16: Redis cache, `/nope` (no click) | 34,759 | 1.3 ms | 2.8 ms | 8x faster: the click UPDATE was the ceiling |
| 17: clicks batched, `/1` | 34,291 | 1.3 ms | 3.4 ms | 7.7x faster than Step 16; within 1% of the no-click ceiling |

Throughput = concurrency / latency (50 / 0.0014 s is about 35,700/s), so
`hey` numbers can be sanity-checked with Little's law.

Concurrency sweep on `/1` (Step 16, before batching): c=10 gave 4,670/s
(2.1 ms avg), c=50 gave 4,426/s (11.2 ms), c=200 gave 4,291/s (46.4 ms).
Throughput stayed flat while latency grew linearly with concurrency, the
signature of a serialized queue: every request updated the same row, and
updates to one row run one after another.

Findings:
- Removing the SELECT (the cache) did nothing for throughput, so the SELECT
  was never the bottleneck.
- Moving the click write off the hot path was the real win.
- With the hot row gone, the new ceiling is some mix of Redis, the Go
  server, `hey`, and Docker's network layer on one laptop. Step 20
  (several instances) is meant to tell which.

## Known limitations

- Generated codes are sequential, so links can be enumerated.
- Aliases are case-sensitive (`My-Link` and `my-link` differ).
- No rate limiting, authentication, or link expiry yet. Clicks are counted
  per link; there are no per-click events (timestamp, referrer) yet.
- Validation checks URL shape only, not whether the site exists or is safe.
- Click counts are held in memory and flushed every `CLICK_FLUSH_INTERVAL`.
  A graceful stop loses nothing; a hard crash (SIGKILL, power loss) loses
  up to one interval of clicks. `Clicks` read via `Get` is eventually
  consistent. This is fine for popularity counters, not for billing.
- While Postgres is down, failed click flushes log a line every second.
- One app instance, one Redis, one Postgres. It runs locally under Docker
  Compose and is not deployed anywhere yet.

## Roadmap

| Step | Planned |
|---|---|
| 18 | Rate limiting: token bucket in a Redis Lua script, shared across instances |
| 19 | Click analytics events and a distributed ID generator (block allocation or Snowflake-style) |
| 20 | Several app instances behind nginx; health checks; load tests |
| 21 | Read replicas and sharding discussion |

## Step log

### Step 1: Data model (`internal/store/store.go`)
- `Link` mirrors one row of the `links` table.
- Store only the **code** (`aB3xYz`), not the full short URL. The domain is
  configuration, not data.
- `Store` started with three methods: `Create`, `Get`, `IncrementClicks`.
  Later steps added `CreateWithCode` (Step 13), `Resolve` (Step 16) and
  `AddClicks` (Step 17).
- `ErrNotFound` is a sentinel error so handlers can return 404 using
  `errors.Is`.
- Go convention: `context.Context` is the first parameter of anything that
  does I/O; initialisms are capitalised (`ID`, `URL`).

### Step 2: Database schema (`migrations/001_create_links.sql`)
- `id BIGSERIAL PRIMARY KEY`: auto-incrementing 64-bit ID.
- `code TEXT UNIQUE`: nullable because the code is derived from the ID,
  which only exists after the insert (insert, then set code). `UNIQUE` is a
  safety net.
- `clicks BIGINT NOT NULL DEFAULT 0`, `created_at TIMESTAMPTZ DEFAULT now()`.

### Step 3: Local Postgres with Docker
- `docker compose up -d` starts `postgres:16`; data lives in a named volume.
- Host port is **5433**, so the connection string is
  `postgres://shortener:shortener@localhost:5433/shortener`.
- Load schema: `docker compose exec -T db psql -U shortener -d shortener < migrations/001_create_links.sql`

### Step 4: Connecting from Go (`cmd/server/main.go`)
- Create **one** `pgxpool` at startup and share it.
- Read `DATABASE_URL` from the environment, with a local default.
- **Lessons learned:**
  - Env var names can't contain spaces (`DATABASE_URL`, not `DATABASE URL`).
  - Go doesn't force you to check errors. An ignored `Ping` error printed
    "connected" even when it wasn't.
  - Always check that a check can fail: stop the DB and confirm the program
    errors.

### Step 5: Base62 encoding (`internal/shortcode/base62.go`)
- A base62 code is the row ID written in a number system with 62 symbols
  (`0-9a-zA-Z`), like decimal but with more symbols.
- Algorithm: repeatedly take `id % 62` (the next digit, rightmost first)
  and `id / 62` until the quotient is 0; then reverse the digits.
- Why encode the ID instead of random strings: IDs are unique, so codes
  can never collide and no retry logic is needed.
- Why base62: URL-safe (no `+` or `/`) and compact. 6 chars cover ~56.8
  billion IDs.
- Caveats: codes are case-sensitive, and sequential IDs are guessable.
- Tested with a table-driven test (a slice of `{input, want}` structs
  looped over). Hand-verified cases: 0 -> "0", 1 -> "1", 61 -> "Z",
  62 -> "10", 125 -> "21".
- Go caches passing tests; use `go test -count=1` to force a rerun.
- Lesson: `string(int)` gives a Unicode character, not digits. Index into
  an alphabet instead.

### Step 6: Postgres store (`internal/store/postgres.go`)
- `PostgresStore` holds the pool and implements `Store`. Go interfaces are
  satisfied implicitly; `var _ Store = (*PostgresStore)(nil)` is a
  compile-time check that it still does.
- `New...` functions are Go's convention for constructors.
- Queries use placeholders (`$1`) so values never get interpreted as SQL
  (prevents SQL injection).
- `QueryRow(...).Scan(&field)` reads one row into pointers; `Exec` runs
  statements with no result rows, and `RowsAffected()` tells how many matched.
- `pgx.ErrNoRows` is translated to our `ErrNotFound` so handlers don't
  depend on the driver.
- `Create` needs two statements (the code is derived from the generated ID),
  so it runs in a transaction (`pgx.BeginFunc`): both succeed or both roll
  back.
- Click counting is `clicks = clicks + 1` in SQL, which is atomic; doing
  read-modify-write in Go would lose counts under concurrency.
- Errors are wrapped with `fmt.Errorf("...: %w", err)` to add context while
  keeping `errors.Is` working.

### Step 7: Manual test of the store (`cmd/server/main.go`)
- Before building HTTP, exercised the store directly: Create, Get,
  IncrementClicks, then Get for a missing code.
- Holding the store in a `store.Store` variable (the interface type) keeps
  the calling code independent of Postgres.
- `errors.Is(err, store.ErrNotFound)` works because handlers/tests compare
  against our own sentinel error.
- First row gets ID 1 -> code "1"; codes grow as IDs grow (base62 end to end).
- This test code was temporary and was replaced by the real server.

### Step 8: Input validation (`internal/handler/validate.go`)
- Bad input is a **400 Bad Request** (`ErrInvalidURL`); a missing code on
  lookup is a **404** (`store.ErrNotFound`). Different failures, different
  errors.
- Allowlist, don't blocklist: only `http`/`https` with a non-empty host.
  `javascript:` and `data:` URLs are code, not pages.
- Rules: trim and reject empty, length cap (2048), `url.Parse`, scheme
  check, host check (`Hostname()` so `http://:8080` is caught).
- `url.Parse` lowercases the scheme, so no manual case handling is needed.
- `ValidateURL` returns the trimmed string; store that, not the raw input.
- Errors are wrapped with `%w` so `errors.Is(err, ErrInvalidURL)` works
  while the message still says why.
- Validation checks shape only; it does not check that the site exists or
  is safe.
- `url.Parse` splits a URL into Scheme, Host, Path, RawQuery, Fragment.
  It is lenient: `example.com`, `javascript:alert(1)`, and `http://` all
  parse without error. A nil error only means "recognizable structure",
  so we check scheme and hostname ourselves afterwards.
- Tests: table-driven with `wantErr` meaning "should be rejected". The
  check compares `err != nil` to `wantErr`; passing tests stay silent.
  Rejections must wrap `ErrInvalidURL`. Lesson: a test that only reports
  errors, without comparing to the expectation, can't tell a correct
  rejection from a bug.

### Step 9: HTTP handlers (`internal/handler/handler.go`, `cmd/server/main.go`)
- `Handler` struct holds the `Store` interface and base URL; handlers are
  methods on it (same pattern as `PostgresStore`).
- Routing with Go 1.22+ `ServeMux`: `POST /shorten`, `GET /{code}`;
  `r.PathValue("code")` reads the wildcard.
- `Shorten`: cap body (`MaxBytesReader`), decode JSON, validate (400),
  store the cleaned URL, respond 201. Errors from our own failures are
  logged and answered with a generic 500 so internals don't leak.
- `Redirect`: lookup (404 on `ErrNotFound`), count the click, 302 redirect.
  (Since Step 16 the lookup is `Resolve`, and since Step 17 the click is
  recorded in memory instead of written to Postgres on the request path.)
  - **302 vs 301**: browsers cache 301, so repeat visits would skip our
    server and never be counted. 302 always comes back.
- `writeJSON`: set headers before `WriteHeader`; later header changes are
  ignored.
- `main.go` only wires layers (pool -> store -> handler) and reads config
  from env. It uses an explicit `http.Server` with `ReadHeaderTimeout` to
  resist slow-client attacks.
- `log.Fatal` skips deferred calls (fixed in Step 11).
- Verified with curl: valid shorten -> 201 + short URL; redirect -> 302 with
  `Location`; `javascript:` URL -> 400; invalid JSON -> 400; unknown code ->
  404. `lsof -i :8080` shows what is listening on a port; a "connection
  refused" from curl means no server is running there.
- `http.Redirect` also writes a small HTML body for clients that don't
  follow redirects.
- Logs: Go's `log` package writes to **stderr** of the process, i.e. the
  terminal running the server. Clients never see them: the client gets a
  generic error, the real reason is in the server log. (An access log was
  added in Step 12.)

### Step 10: Handler tests with a fake store (`internal/handler/handler_test.go`)
- `Handler` depends on the `Store` interface, so tests use an in-memory
  `fakeStore`: no database or Docker, runs in milliseconds.
- The fake has an `err` field to simulate a database outage, which is hard
  to trigger with real Postgres. It returns the same `store.ErrNotFound`
  sentinel as the real store so the handler's `errors.Is` check works.
- Maps hold copies of structs: read, modify, write back
  (`f.links[code].Clicks++` does not compile).
- `httptest.NewRequest` + `httptest.NewRecorder` call the handler as a plain
  function: no server, no port. Requests go through `Handler.Routes()` so
  the real routing (including `r.PathValue`) is tested; calling
  `h.Redirect` directly would leave `PathValue` empty.
- Each test builds a fresh fake so tests never share state.
- Cases: shorten OK, stores the trimmed URL, bad input (400, nothing
  stored), redirect OK (302 + `Location` + click recorded), every visit
  counted, unknown code (404, not counted), store failure (500 that
  doesn't leak the internal error).
- `t.Fatalf` stops a test when later checks are meaningless; `t.Errorf`
  records and continues.
- Verified the tests can fail by breaking the handler on purpose.

### Step 11: Graceful shutdown (`cmd/server/main.go`)
- `signal.NotifyContext` turns SIGINT (Ctrl+C) and SIGTERM (sent by
  Docker / Kubernetes / systemd) into a cancelled context.
- `srv.Shutdown(ctx)` stops accepting new connections and waits for
  in-flight requests, up to a deadline (10s here).
- `ListenAndServe` blocks, so it runs in a goroutine; `select` waits for
  either a server error or a shutdown signal. The error channel is
  buffered (size 1) so the goroutine's send can never block forever.
- The shutdown deadline uses a fresh `context.Background()` because the
  signal context is already cancelled.
- After `Shutdown`, `ListenAndServe` returns `http.ErrServerClosed`: the
  normal, expected result, not a failure.
- `log.Fatal` calls `os.Exit` and skips defers, so all work moved to
  `run() error`; `main` just calls `log.Fatal(run())`. `pool.Close()` now
  runs on every exit path.
- Calling `stop()` after the first signal restores default behavior, so a
  second Ctrl+C force-kills the process.
- Verified in-flight handling: with a 10s sleep in `Redirect`, Ctrl+C
  about 9s into a request printed "shutting down...", the request still
  completed with its 302 (response header `Connection: close`, which Go
  adds during shutdown), and "server stopped cleanly" appeared only after
  the response was sent.
- The 10s shutdown deadline is a real limit: requests that need longer are
  cut off and `Shutdown` returns "context deadline exceeded".
- Testing lesson: a test must exercise the thing it claims to prove.
  Ctrl+C on an idle server only proves the idle path; compare timestamps
  to be sure of the order of events.
- Gotchas: a server left running from an older build still holds port
  8080 ("address already in use"); `lsof -i :8080` finds the PID and
  `kill <PID>` stops it. The Docker container doesn't restart by itself
  after Docker restarts or the Mac sleeps; `docker compose up -d` brings
  it back, and `restart: unless-stopped` in the compose file avoids it.

### Step 12: Access log middleware (`internal/handler/middleware.go`)
- Middleware = `func(http.Handler) http.Handler`: wraps a handler to run
  code before and after it.
- `http.ResponseWriter` can't report the status afterwards, so
  `statusRecorder` embeds it and overrides `WriteHeader` to remember the
  code and forward the call (skipping the forward would break responses).
- Default status is 200 because Go sends 200 when a handler never calls
  `WriteHeader`. In this app every path calls it explicitly, so the
  default only matters for body-only handlers (covered by a test).
- Pass the recorder, not the original writer, to `next.ServeHTTP`.
- Wrapping the whole router in `main.go` also logs 404s from unmatched
  routes.
- Log method, path, status, duration. Use `r.URL.Path`, not the full URL:
  query strings and bodies can contain secrets.
- Tests: the recorder saves and forwards; the middleware logs the right
  status, defaults to 200, and omits the query string. Log output is
  captured by swapping `log.SetOutput` (global state, so no `t.Parallel`);
  `t.Cleanup` restores it. A cleaner design injects a logger.
- Verified live: one log line per request (201 / 302 / 404 / 400),
  matching the status `curl -i` shows. The log appears in the server's
  terminal, not the client's.

### Step 13: Custom aliases
- Optional `alias` in `POST /shorten`; invalid -> 400, taken -> 409.
- Alias rules (`ValidateAlias`): 3-32 chars, only `A-Z a-z 0-9 - _`, at
  least one `-` or `_`, not a reserved word (case-insensitive). The
  `-`/`_` rule guarantees an alias can never equal a generated base62
  code (base62 has neither character), so a normal `Create` can't collide
  with an alias later.
- Interface: separate `CreateWithCode` method, so `Create` and its tests
  are untouched. A single INSERT, so no transaction is needed.
- Race-free uniqueness: rely on the `UNIQUE` constraint and translate
  Postgres error `23505` (`*pgconn.PgError`, constraint `links_code_key`)
  into `ErrAliasTaken` with `errors.As`, instead of check-then-insert
  (two requests could both pass the check).
- A taken alias is the client's conflict (409), not a server failure (500).
- Handler tests use the fake store, which returns `ErrAliasTaken` by
  construction: they prove the 409 mapping, not the Postgres translation
  (that needs a real-database test, Step 14).
- Verified against real Postgres with curl: alias create -> 201, redirect
  -> 302, duplicate alias -> 409 (proves the `23505` translation, which
  the fake-store tests can't), alias without `-`/`_` -> 400.
- Debugging lesson: a stale server (not restarted after code changes)
  silently ignored the new `alias` field, because `encoding/json` drops
  unknown fields. `DisallowUnknownFields()` turns that into a 400.
- Caveats: failed inserts consume a `BIGSERIAL` value (gaps in IDs are
  harmless); aliases are case-sensitive (`My-Link` != `my-link`).

### Step 14: Integration tests for `PostgresStore` (`internal/store/postgres_test.go`)
- Unit tests (fake store) prove handler logic; integration tests against
  real Postgres prove the SQL, the transaction, and the error translations
  (`pgx.ErrNoRows` -> `ErrNotFound`, `RowsAffected == 0` -> `ErrNotFound`,
  `23505` -> `ErrAliasTaken`). You need both.
- Tests are skipped unless `TEST_DATABASE_URL` is set, so `go test ./...`
  works without Docker. A package where every test skips still prints
  `ok`: that only proves the skip logic, not the SQL.
- One-time setup:
  ```
  docker compose exec db psql -U shortener -d shortener -c "CREATE DATABASE shortener_test"
  docker compose exec -T db psql -U shortener -d shortener_test < migrations/001_create_links.sql
  ```
- Each test starts with `TRUNCATE links RESTART IDENTITY`, so the first
  row of every test gets ID 1. The helper refuses to run unless
  `current_database()` ends in `_test`, so a wrong URL can't wipe real data.
- No `t.Parallel()`: tests share one table.
- Concurrency test: 50 goroutines x 10 increments must give exactly 500
  (passed). `clicks = clicks + 1` in SQL is atomic.
- `t.Fatalf` only from the test's own goroutine; use `t.Errorf` in
  goroutines. Compare `time.Time` with `Equal`, not `==`.
- Result: all integration tests pass against the real database.

### Step 15: Containerize the app (`Dockerfile`, `docker-compose.yaml`)
- Multi-stage build: compile with the Go toolchain image, copy only the
  static binary (`CGO_ENABLED=0`) into `distroless/static:nonroot`. The
  final image is about 20 MB instead of about 1 GB, has no shell, and runs
  as a non-root user.
- The build image tag must satisfy the `go` line in `go.mod`
  (`golang:1.26` for `go 1.26.5`), or the build fails.
- Copy `go.mod`/`go.sum` and run `go mod download` before copying the
  source, so Docker caches dependencies and code edits rebuild fast.
- `ENTRYPOINT` in exec form makes the binary PID 1 so it receives SIGTERM
  directly; shell form would swallow the signal and break graceful
  shutdown. `stop_grace_period` is longer than the app's 10s shutdown
  deadline.
- Inside the Compose network services talk by name on the container port
  (`db:5432`); `localhost:5433` is only for the host (TablePlus, tests).
  `localhost` inside a container is the container itself, and a published
  port only exists on the host.
- `depends_on` with `condition: service_healthy` and a `pg_isready`
  healthcheck makes the app wait for a ready database, not just a started
  container.
- Config came from env vars, so no app code changed.
- Redis runs on the Compose network only (no published port).
- Verified: image is 19.6 MB on disk; a new link got code 9 (the volume
  persisted rows 1-8 across the rebuild); `docker compose stop app` took
  0.1s and logged "shutting down..." / "server stopped cleanly".
- Wrong-port experiment: `db:5433` fails with connection refused even
  though DNS resolves `db` (5433 is the host-side mapping; use 5432 inside
  the network). With `restart: unless-stopped`, the failing DB ping made the
  app exit and Docker restart it with growing delays (a crash loop with
  backoff). Better: retry the connection inside the app before exiting.
- Gotcha: a leftover `go run` server held port 8080, so the container
  failed with "address already in use". `lsof -i :8080` finds it.

### Step 16: Redis cache (`internal/store/cached.go`)
- `CachedStore` is a decorator around any `Store`: handlers don't change.
  Embedding the `Store` interface passes `Get`/`IncrementClicks`/`AddClicks`
  through; only `Resolve`, `Create` and `CreateWithCode` are overridden.
- New narrow method `Resolve(code) -> url`: the redirect hot path. A cached
  `Link` would carry a stale `Clicks`, so only the URL is cached.
- Cache-aside on `Resolve`; write-through on `Create`/`CreateWithCode`.
- TTL 1h with +/-10% jitter so keys written together don't expire together.
- Negative caching (30s): scanners probing random codes don't reach
  Postgres. Written with `SetNX` so it can never overwrite a real link, and
  `CreateWithCode` overwrites any stale marker (otherwise a new alias would
  404 until the marker expired).
- Redis failure = cache miss, never an error; 100ms timeout per Redis call
  and `MaxRetries: -1` so a hung cache can't slow redirects.
- `singleflight` collapses concurrent misses on a hot key into one DB
  query (cache stampede); the shared call uses `context.WithoutCancel` so
  one cancelled request can't fail everyone waiting, and double-checks the
  cache before querying.
- Redis runs with `--maxmemory 64mb --maxmemory-policy allkeys-lru`.
- `REDIS_ADDR` empty = no cache. Redis down at startup = warning only.
- Tests use miniredis (fake Redis, TTL fast-forward) and a counting inner
  store: hit, TTL, negative caching and expiry, stale-404 bug, write-through,
  Redis-down fallback, stampede (50 goroutines -> 1 query). `TestResolve`
  checks the real SQL against Postgres.
- Hit rate during benchmarking: 60,004 hits / 2 misses (the 2 misses are
  one cold key: singleflight double-checks the cache, so a cold key counts
  twice).
- Caching removed the SELECT but throughput did not change (see
  Benchmarks): each click still did a synchronous UPDATE on one row.
- Side effects: cached links keep redirecting while Postgres is down, and
  redirects keep working (from Postgres) while Redis is down.

### Step 17: Click counting off the hot path (`internal/clicks/counter.go`)
- Finding from the Step 16 benchmarks: every redirect did `UPDATE ...
  clicks + 1` on the same row, and updates to one row serialize. A request
  that skipped the UPDATE (`/nope`) ran 8x faster.
- `Counter` aggregates in memory: `Record(code)` is a locked map increment
  (nanoseconds, no I/O). Memory is bounded by distinct codes per interval,
  not by clicks, so there is no "queue full" case to handle.
- A background loop flushes every second (`CLICK_FLUSH_INTERVAL`) with ONE
  statement for all codes: `UPDATE links SET clicks = clicks + d.n FROM
  unnest($1::text[], $2::bigint[]) AS d(code, n) WHERE links.code =
  d.code`. 20,000 clicks on a hot link become one row update per second.
- `Flush` swaps the map under the lock and writes OUTSIDE it, so a slow
  database never blocks `Record`. On failure the counts are merged back
  and retried on the next tick (nothing lost, memory still bounded).
- Shutdown order: `srv.Shutdown` first (no more requests), then
  `counter.Close` (final flush). Implemented with `defer` ordering in
  `run()`; `Close` is idempotent (`sync.Once`).
- `clicks = clicks + n` is commutative, so several instances can flush
  into the same row without overwriting each other. Concurrent flushes of
  overlapping codes can deadlock occasionally; the retry covers it.
- The handler depends on a tiny `ClickRecorder` interface, so tests use a
  fake; 404s are never counted.
- Tests: aggregation into one batch, empty flush doesn't call the sink,
  `Record` never blocks during a stuck flush, failure merge-back, `Close`
  flushes, background timer, concurrent `Record` under `-race`, and a real
  Postgres test for `AddClicks` (adds, ignores unknown codes, empty no-op).
- Verified: the counter went 160,010 -> 180,010 across a 20,000-request
  run (exactly +20,000, nothing lost or double-counted).
- Graceful stop (`docker compose stop app`): 198,891 -> 218,891, exactly
  +20,000, and the log shows "server stopped cleanly" BEFORE "click counter
  flushed", confirming the shutdown ordering.
- Hard crash (`docker compose kill app` right after a 0.64s burst): 1,119
  of 20,000 clicks lost (5.6%): whatever arrived after the last 1-second
  flush tick was only in memory. The loss is bounded by one flush interval
  of traffic, not a percentage; a shorter interval shrinks it at the cost of
  more (still batched) UPDATEs.
- Trade-offs: a graceful stop loses nothing; a hard crash loses up to one
  interval. Fine for popularity counters, not for billing.

  ### Step 19: Click analytics (`internal/clicks/stats.go`, `internal/store/stats.go`)
- Aggregate before you store: clicks are grouped in memory by (code, hour,
  referrer host, device class) and flushed in batches with one
  `INSERT ... ON CONFLICT DO UPDATE SET clicks = clicks + n`. Write volume
  depends on the number of distinct groups, not the number of clicks (one
  row per click would bring back the Step 16 bottleneck).
- Privacy: no IP addresses; the Referer is reduced to its host (the full
  URL can hold private paths and tokens); the User-Agent is reduced to
  desktop / mobile / bot / other.
- The Referer is attacker-controlled, so cardinality is capped (10,000
  groups per flush interval); extra referrers fold into "(other)" instead
  of growing the map. The table can still grow by up to that cap per flush
  under sustained abuse: enable the redirect rate limit and add a
  retention job if exposed publicly.
- Same machinery as Step 17: `Record` is a locked map increment, `Flush`
  swaps the map and writes outside the lock, failures merge back, `Close`
  flushes after `srv.Shutdown`. Several instances can flush the same group
  because the upsert adds.
- `GET /stats/{code}?hours=24` (1-720): total, clicks per hour, top
  referrers, devices. `STATS_TOKEN` makes it require
  `Authorization: Bearer <token>` (constant-time compare); without it the
  endpoint is public to anyone who knows a code.
- `Store` interface unchanged: stats methods live on `PostgresStore` and
  reach the handler through `UseStats`. Stats bypass the Redis cache.
- Totals (`links.clicks`) and breakdowns (`click_stats`) are two
  independent pipelines; they agree after a flush (verified: both 60).
- Tests: classification tables, aggregation and hour split, failure
  merge-back, close flush, cardinality cap, concurrent record under
  `-race`, handler routes (token, hours validation, no leak on 500), and a
  real-Postgres test for the upsert, unknown codes and the summary queries.