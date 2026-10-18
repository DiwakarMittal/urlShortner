# URL Shortener (Go + Postgres)

A learning project: a service that turns long URLs into short codes and
redirects visitors to the original page.

## How it works

1. **Create**: client sends a long URL, server stores it, returns a short code.
2. **Redirect**: visitor opens `/<code>`, server looks it up and replies
   with a redirect (`302` + `Location` header) to the long URL.

The short link is only a pointer. The original page is unchanged, and the
shortener server must be publicly reachable for others to use the links.

## Quick start
```
docker compose up -d
docker compose exec -T db psql -U shortener -d shortener < migrations/001_create_links.sql
go run ./cmd/server
```

Try it:

```
curl -i -X POST localhost:8080/shorten -d '{"url":"https://example.com"}'
curl -i localhost:8080/<code>
```

Run all tests: `go test -count=1 ./...`

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
cmd/server/main.go              entry point: config, DB, graceful shutdown
internal/store/store.go         Link struct, Store interface, ErrNotFound
internal/store/postgres.go      Postgres implementation
internal/handler/handler.go     HTTP handlers and routes
internal/handler/validate.go    URL validation
internal/handler/*_test.go      handler + validation tests (fake store)
internal/shortcode/base62.go    base62 encoding (+ test)
migrations/001_create_links.sql schema
docker-compose.yaml             local Postgres
```
### Inspecting the database with a GUI (TablePlus)
- Connection: PostgreSQL, host `127.0.0.1`, port **5433** (the host side
  of the Docker port mapping), user/password/database `shortener`.
- Browse rows under `public` -> `links` (Data tab) and the schema in the
  Structure tab. Refresh (Cmd+R) to see new clicks; it doesn't live-update.
- Manual edits and `UPDATE`/`DELETE` here affect real data; always use a
  `WHERE`.

`internal/` is enforced by Go: other modules cannot import these packages.

## Step log

### Step 1: Data model (`internal/store/store.go`)
- `Link` mirrors one row of the `links` table.
- Store only the **code** (`aB3xYz`), not the full short URL. The domain is
  configuration, not data.
- `Store` has three methods: `Create`, `Get`, `IncrementClicks`.
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
- This test code is temporary and will be replaced by the real server.

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
- `log.Fatal` skips deferred calls; graceful shutdown is a later upgrade.
- Manual test with `curl -i`: 201 / 302 / 400 / 404 all verified.
- Verified with curl: valid shorten -> 201 + short URL; redirect -> 302 with
  `Location`; `javascript:` URL -> 400; invalid JSON -> 400; unknown code ->
  404. `lsof -i :8080` shows what is listening on a port; a "connection
  refused" from curl means no server is running there.
- `http.Redirect` also writes a small HTML body for clients that don't
  follow redirects.
- `log.Fatal` skips deferred calls (fixed in Step 11).
- Logs: Go's `log` package writes to **stderr** of the process, i.e. the
  terminal running the server. Clients never see them: the client gets a
  generic error, the real reason is in the server log. No access log yet.

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