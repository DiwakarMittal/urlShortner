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

### Step 6: the Postgres store (internal/store/postgres.go)