# URL Shortener (Go + Postgres)

A learning project: a service that turns long URLs into short codes and
redirects visitors to the original page.

## How it works

1. **Create**: client sends a long URL, server stores it, returns a short code.
2. **Redirect**: visitor opens `/<code>`, server looks it up and replies
   with a redirect (`302` + `Location` header) to the long URL.

The short link is only a pointer. The original page is unchanged, and the
shortener server must be publicly reachable for others to use the links.

## API

| Request | Success | Errors |
|---|---|---|
| `POST /shorten` with `{"url": "...", "alias": "optional-name"}` | `201` `{"short_url": "..."}` | `400` bad JSON / URL / alias, `409` alias taken, `500` |
| `GET /{code}` | `302` redirect to the long URL (click counted) | `404` unknown code, `500` |

## Quick start

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

Run all tests: `go test -count=1 ./...`

Integration tests (real Postgres; see Step 14 for the one-time setup):

```
TEST_DATABASE_URL="postgres://shortener:shortener@localhost:5433/shortener_test" go test -count=1 -v ./internal/store/
```

| Env var | Default |
|---|---|
| `DATABASE_URL` | `postgres://shortener:shortener@localhost:5433/shortener` |
| `BASE_URL` | `http://localhost:8080` |
| `ADDR` | `:8080` |

## Tech choices (and why)

| Choice | Why |
|---|---|
| Go `net/http` | Standard library is enough; Go 1.22+ `ServeMux` supports `GET /{code}` patterns, so no router needed |
| Postgres | Production-grade; `BIGSERIAL` gives unique IDs and `UPDATE ... clicks + 1` is atomic |
| `pgx` + `pgxpool` | Modern Postgres driver; the pool reuses connections instead of opening one per request |
| Base62 of the row ID | IDs are unique by construction, so no collision checks or retries |
| `Store` interface | Handlers depend on an interface, not Postgres: simple handlers, swappable DB, fakes in tests |

## Project structure

```
cmd/server/main.go                 entry point: config, DB, graceful shutdown
internal/store/store.go            Link struct, Store interface, errors
internal/store/postgres.go         Postgres implementation
internal/store/postgres_test.go    integration tests (need TEST_DATABASE_URL)
internal/handler/handler.go        HTTP handlers and routes
internal/handler/validate.go       URL and alias validation
internal/handler/middleware.go     access log middleware
internal/handler/*_test.go         handler, validation, middleware tests
internal/shortcode/base62.go       base62 encoding (+ test)
migrations/001_create_links.sql    schema
docker-compose.yaml                local Postgres
```

`internal/` is enforced by Go: other modules cannot import these packages.

## Inspecting the database (TablePlus)

- Connection: PostgreSQL, host `127.0.0.1`, port **5433** (the host side
  of the Docker port mapping), user/password/database `shortener`.
- Browse rows under `public` -> `links` (Data tab) and the schema in the
  Structure tab. Refresh (Cmd+R) to see new clicks; it doesn't live-update.
- Manual edits and `UPDATE`/`DELETE` here affect real data; always use a
  `WHERE`.

## Known limitations

- Generated codes are sequential, so links can be enumerated.
- Aliases are case-sensitive (`My-Link` and `my-link` differ).
- No rate limiting, authentication, link expiry, or per-click analytics.
- Validation checks URL shape only, not whether the site exists or is safe.
- Click counting is synchronous (one extra DB write per redirect).
- Runs locally; not yet deployed or containerized.

## Step log

### Step 1: Data model (`internal/store/store.go`)
- `Link` mirrors one row of the `links` table.
- Store only the **code** (`aB3xYz`), not the full short URL. The domain is
  configuration, not data.
- `Store` started with three methods: `Create`, `Get`, `IncrementClicks`
  (a fourth, `CreateWithCode`, arrives in Step 13).
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
  looped over). Hand-verified cases: 0→"0", 1→"1", 61→"Z", 62→"10",
  125→"21".
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
  - **302 vs 301**: browsers cache 301, so repeat visits would skip our
    server and never be counted. 302 always comes back.
  - A failed click count is logged but doesn't block the redirect.
- `writeJSON`: set headers before `WriteHeader`; later header changes are
  ignored.
- `main.go` only wires layers (pool -> store -> handler) and reads config
  from env (`DATABASE_URL`, `BASE_URL`, `ADDR`). It uses an explicit
  `http.Server` with `ReadHeaderTimeout` to resist slow-client attacks.
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
  stored), redirect OK (302 + `Location` + click counted), every visit
  counted, unknown code (404), store failure (500 that doesn't leak the
  internal error).
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
  terminal, not the client's. `go test -count=1 ./...` passes.

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
- Each test starts with `TRUNCATE links RESTART IDENTITY` so IDs begin at
  1. The helper refuses to run unless `current_database()` ends in
  `_test`, so a wrong URL can't wipe real data.
- No `t.Parallel()`: tests share one table.
- Concurrency test: 50 goroutines x 10 increments must give exactly 500
  (passed). `clicks = clicks + 1` in SQL is atomic.
- `t.Fatalf` only from the test's own goroutine; use `t.Errorf` in
  goroutines. Compare `time.Time` with `Equal`, not `==`.
- Result: all 9 integration tests pass against the real database.

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
  `localhost` inside a container is the container itself.
- `depends_on` with `condition: service_healthy` and a `pg_isready`
  healthcheck makes the app wait for a ready database, not just a started
  container.
- Config came from env vars (`DATABASE_URL`, `BASE_URL`, `ADDR`), so no app
  code changed.
- Redis runs on the Compose network only (no published port); it is used
  from Step 16.
- Gotcha: a leftover `go run` server held port 8080, so the container
  failed with "address already in use". `lsof -i :8080` finds it. Use
  either `go run` or the container, not both.