# URL Shortener (Go + Postgres)

A learning project: a service that turns long URLs into short codes and
redirects visitors to the original page.

## How it works

1. **Create**: client sends a long URL, server stores it, returns a short code.
2. **Redirect**: visitor opens `/<code>`, server looks it up and replies
   with a redirect (`302` + `Location` header) to the long URL.

The short link is only a pointer. The original page is unchanged, and the
shortener server must be publicly reachable for others to use the links.

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
cmd/server/main.go        entry point: config, DB connection, start server
internal/store/           Link struct, Store interface, Postgres implementation
internal/handler/         HTTP handlers
internal/shortcode/       base62 encoding
migrations/               SQL schema
docker-compose.yaml       local Postgres
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

### Step 5: the Postgres store (internal/store/postgres.go)
- Tested with a table-driven test (a slice of `{input, want}` structs
  looped over). Hand-verified cases: 0→"0", 1→"1", 61→"Z", 62→"10", 125→"21".
- Go caches passing tests; use `go test -count=1` to force a rerun.
- Lesson: `string(int)` gives a Unicode character, not digits. Index into an
  alphabet instead.

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