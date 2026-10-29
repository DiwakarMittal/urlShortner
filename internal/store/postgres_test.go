package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestStore connects to the TEST database and empties the table, so every
// test starts from a known state (IDs begin at 1 again).
//
// These tests talk to a real Postgres, so they are skipped unless
// TEST_DATABASE_URL is set. That keeps `go test ./...` green for anyone
// without Docker.
func newTestStore(t *testing.T) *PostgresStore {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close) // runs when the test ends, even if it fails

	// Safety check: TRUNCATE wipes the table, so refuse to run unless the
	// database is clearly a test one. This protects your real data if
	// TEST_DATABASE_URL ever points at the wrong place.
	var dbName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
		t.Fatalf("query database name: %v", err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("refusing to run: database %q does not end in _test", dbName)
	}

	// RESTART IDENTITY resets the BIGSERIAL counter, so the first row in
	// each test gets ID 1 (and therefore code "1").
	if _, err := pool.Exec(ctx, "TRUNCATE links, click_stats RESTART IDENTITY"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	return NewPostgresStore(pool)
}

// Create must return the generated ID and code, and Get must read the same
// row back. This exercises the INSERT, the UPDATE and the transaction.
func TestCreateAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.Create(ctx, "https://example.com/page")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != 1 || created.Code != "1" {
		t.Errorf("Create returned ID=%d Code=%q, want ID=1 Code=%q", created.ID, created.Code, "1")
	}
	if created.Clicks != 0 {
		t.Errorf("new link Clicks = %d, want 0", created.Clicks)
	}
	if created.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero; Postgres should have set it")
	}

	got, err := s.Get(ctx, created.Code)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID || got.Code != created.Code || got.LongURL != "https://example.com/page" {
		t.Errorf("Get returned %+v, want the link that Create returned (%+v)", got, created)
	}
	// time.Time must be compared with Equal, not ==.
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt differs: Get %v, Create %v", got.CreatedAt, created.CreatedAt)
	}
}

// Codes must follow the base62 encoding of the ID, end to end:
// 1 -> "1", 61 -> "Z", 62 -> "10".
func TestCreate_CodeMatchesID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := map[int64]string{1: "1", 61: "Z", 62: "10"}

	for id := int64(1); id <= 62; id++ {
		link, err := s.Create(ctx, "https://example.com")
		if err != nil {
			t.Fatalf("Create #%d: %v", id, err)
		}
		if link.ID != id {
			t.Fatalf("Create #%d returned ID %d", id, link.ID)
		}
		if code, ok := want[id]; ok && link.Code != code {
			t.Errorf("ID %d got code %q, want %q", id, link.Code, code)
		}
	}
}

// A missing code must become our ErrNotFound, not a raw pgx error.
func TestGet_NotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Get(context.Background(), "nope")

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestIncrementClicks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	link, err := s.Create(ctx, "https://example.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := s.IncrementClicks(ctx, link.Code); err != nil {
			t.Fatalf("IncrementClicks: %v", err)
		}
	}

	got, err := s.Get(ctx, link.Code)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Clicks != 2 {
		t.Errorf("Clicks = %d, want 2", got.Clicks)
	}
}

// UPDATE with no matching row is not an error in SQL, so the store must
// notice RowsAffected() == 0 and return ErrNotFound itself.
func TestIncrementClicks_NotFound(t *testing.T) {
	s := newTestStore(t)

	err := s.IncrementClicks(context.Background(), "nope")

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("IncrementClicks(missing) error = %v, want ErrNotFound", err)
	}
}

// The whole point of `clicks = clicks + 1` in SQL: concurrent visitors must
// never lose a count. 50 goroutines x 10 increments must give exactly 500.
func TestIncrementClicks_Concurrent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	link, err := s.Create(ctx, "https://example.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const workers, perWorker = 50, 10

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				if err := s.IncrementClicks(ctx, link.Code); err != nil {
					// Errorf (not Fatalf) is the only safe choice here:
					// Fatalf must be called from the test's own goroutine.
					t.Errorf("IncrementClicks: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait() // block until every goroutine has finished

	got, err := s.Get(ctx, link.Code)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := int64(workers * perWorker); got.Clicks != want {
		t.Errorf("Clicks = %d, want %d (lost updates!)", got.Clicks, want)
	}
}

func TestCreateWithCode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.CreateWithCode(ctx, "https://example.com/target", "my-link")
	if err != nil {
		t.Fatalf("CreateWithCode: %v", err)
	}
	if created.Code != "my-link" || created.ID != 1 {
		t.Errorf("CreateWithCode returned %+v, want Code=my-link ID=1", created)
	}

	got, err := s.Get(ctx, "my-link")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LongURL != "https://example.com/target" {
		t.Errorf("LongURL = %q, want %q", got.LongURL, "https://example.com/target")
	}
}

// The fake store returns ErrAliasTaken by construction. THIS test is the
// one that proves the real Postgres error (23505) is translated correctly.
func TestCreateWithCode_Duplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateWithCode(ctx, "https://original.example", "my-link"); err != nil {
		t.Fatalf("first CreateWithCode: %v", err)
	}

	_, err := s.CreateWithCode(ctx, "https://other.example", "my-link")

	if !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("duplicate error = %v, want ErrAliasTaken", err)
	}
	// The original must be untouched.
	got, err := s.Get(ctx, "my-link")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LongURL != "https://original.example" {
		t.Errorf("original link changed to %q", got.LongURL)
	}
}

// An alias row uses up an ID, but generated codes must keep working after.
func TestCreate_AfterAlias(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateWithCode(ctx, "https://example.com", "my-link"); err != nil {
		t.Fatalf("CreateWithCode: %v", err)
	}

	link, err := s.Create(ctx, "https://example.com/next")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.ID != 2 || link.Code != "2" {
		t.Errorf("Create returned ID=%d Code=%q, want ID=2 Code=%q", link.ID, link.Code, "2")
	}
}

// Resolve must return just the URL, and map a missing code to ErrNotFound.
func TestResolve(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	link, err := s.Create(ctx, "https://example.com/page")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Resolve(ctx, link.Code)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "https://example.com/page" {
		t.Errorf("Resolve = %q, want %q", got, "https://example.com/page")
	}

	if _, err := s.Resolve(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(missing) error = %v, want ErrNotFound", err)
	}
}

// AddClicks must ADD to the stored value (not replace it), ignore unknown
// codes, and treat an empty batch as a no-op.
func TestAddClicks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a, err := s.Create(ctx, "https://a.example")
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := s.Create(ctx, "https://b.example")
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}

	clicksOf := func(code string) int64 {
		t.Helper()
		l, err := s.Get(ctx, code)
		if err != nil {
			t.Fatalf("Get %q: %v", code, err)
		}
		return l.Clicks
	}

	// "nope" does not exist and must be ignored, not an error.
	if err := s.AddClicks(ctx, map[string]int64{a.Code: 5, b.Code: 3, "nope": 2}); err != nil {
		t.Fatalf("AddClicks: %v", err)
	}
	if got := clicksOf(a.Code); got != 5 {
		t.Errorf("a clicks = %d, want 5", got)
	}
	if got := clicksOf(b.Code); got != 3 {
		t.Errorf("b clicks = %d, want 3", got)
	}

	// A second batch adds on top: 5 + 2 = 7, and b is untouched.
	if err := s.AddClicks(ctx, map[string]int64{a.Code: 2}); err != nil {
		t.Fatalf("AddClicks: %v", err)
	}
	if got := clicksOf(a.Code); got != 7 {
		t.Errorf("a clicks = %d, want 7", got)
	}
	if got := clicksOf(b.Code); got != 3 {
		t.Errorf("b clicks = %d, want 3 (must not change)", got)
	}

	if err := s.AddClicks(ctx, map[string]int64{}); err != nil {
		t.Errorf("empty AddClicks: %v", err)
	}
}

// AddStats must ADD to existing groups (upsert), ignore unknown codes, and
// Stats must summarize per hour, referrer and device.
func TestAddStatsAndStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a, err := s.Create(ctx, "https://a.example")
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := s.Create(ctx, "https://b.example")
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}

	h1 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	h2 := h1.Add(time.Hour)

	err = s.AddStats(ctx, []StatRow{
		{a.Code, h1, "google.com", "desktop", 5},
		{a.Code, h1, "", "mobile", 3},
		{a.Code, h2, "google.com", "desktop", 2},
		{b.Code, h1, "x.com", "desktop", 7},
		{"nope", h1, "z.com", "other", 9}, // unknown code: must be ignored
	})
	if err != nil {
		t.Fatalf("AddStats: %v", err)
	}
	// Same group again: must be added to (5 + 1 = 6), not replaced.
	if err := s.AddStats(ctx, []StatRow{{a.Code, h1, "google.com", "desktop", 1}}); err != nil {
		t.Fatalf("second AddStats: %v", err)
	}
	if err := s.AddStats(ctx, nil); err != nil {
		t.Errorf("empty AddStats: %v", err)
	}

	var ignored int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM click_stats WHERE code = 'nope'").Scan(&ignored); err != nil || ignored != 0 {
		t.Errorf("rows for the unknown code = %d, %v; want 0", ignored, err)
	}

	st, err := s.Stats(ctx, a.Code, h1)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Total != 11 {
		t.Errorf("Total = %d, want 11 (6 + 3 + 2)", st.Total)
	}
	if len(st.ByHour) != 2 || !st.ByHour[0].Hour.Equal(h1) || st.ByHour[0].Clicks != 9 ||
		!st.ByHour[1].Hour.Equal(h2) || st.ByHour[1].Clicks != 2 {
		t.Errorf("ByHour = %+v, want h1:9 then h2:2", st.ByHour)
	}
	if len(st.TopReferrers) != 2 || st.TopReferrers[0] != (NameCount{"google.com", 8}) || st.TopReferrers[1] != (NameCount{"(direct)", 3}) {
		t.Errorf("TopReferrers = %+v, want google.com:8 then (direct):3", st.TopReferrers)
	}
	if len(st.Devices) != 2 || st.Devices[0] != (NameCount{"desktop", 8}) || st.Devices[1] != (NameCount{"mobile", 3}) {
		t.Errorf("Devices = %+v, want desktop:8 then mobile:3", st.Devices)
	}

	if st, _ := s.Stats(ctx, a.Code, h2); st.Total != 2 {
		t.Errorf("Total since h2 = %d, want 2", st.Total)
	}
	if st, _ := s.Stats(ctx, b.Code, h1); st.Total != 7 {
		t.Errorf("b Total = %d, want 7 (must not mix with a)", st.Total)
	}

	empty, err := s.Stats(ctx, a.Code, h2.Add(time.Hour))
	if err != nil || empty.Total != 0 || empty.ByHour == nil || len(empty.ByHour) != 0 {
		t.Errorf("empty window = %+v, %v; want Total 0 and a non-nil empty ByHour", empty, err)
	}

	if _, err := s.Stats(ctx, "nope", h1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stats(unknown) error = %v, want ErrNotFound", err)
	}
}
