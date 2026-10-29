package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// StatRow is one aggregated group of clicks, as written by the flush.
type StatRow struct {
	Code     string
	Bucket   time.Time // start of the hour, UTC
	Referrer string    // host only; "" means direct/unknown
	Device   string    // desktop, mobile, bot or other
	Clicks   int64
}

// Stats is the read side: what GET /stats/{code} returns.
type Stats struct {
	Code         string      `json:"code"`
	Since        time.Time   `json:"since"`
	Total        int64       `json:"total"`
	ByHour       []HourCount `json:"by_hour"`
	TopReferrers []NameCount `json:"top_referrers"`
	Devices      []NameCount `json:"devices"`
}

type HourCount struct {
	Hour   time.Time `json:"hour"`
	Clicks int64     `json:"clicks"`
}

type NameCount struct {
	Name   string `json:"name"`
	Clicks int64  `json:"clicks"`
}

// AddStats upserts a batch of rows in ONE statement. ON CONFLICT adds to an
// existing row instead of replacing it, so several app instances can flush
// the same group without overwriting each other. The JOIN against links
// drops rows for unknown codes instead of failing the whole batch.
func (s *PostgresStore) AddStats(ctx context.Context, rows []StatRow) error {
	if len(rows) == 0 {
		return nil
	}

	// A consistent order keeps row locks in the same order across
	// instances (best effort against deadlocks; failed batches are retried).
	sorted := append([]StatRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if !a.Bucket.Equal(b.Bucket) {
			return a.Bucket.Before(b.Bucket)
		}
		if a.Referrer != b.Referrer {
			return a.Referrer < b.Referrer
		}
		return a.Device < b.Device
	})

	n := len(sorted)
	codes := make([]string, n)
	buckets := make([]time.Time, n)
	refs := make([]string, n)
	devs := make([]string, n)
	counts := make([]int64, n)
	for i, r := range sorted {
		codes[i], buckets[i], refs[i], devs[i], counts[i] = r.Code, r.Bucket, r.Referrer, r.Device, r.Clicks
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO click_stats (code, bucket, referrer, device, clicks)
		 SELECT d.code, d.bucket, d.referrer, d.device, d.n
		 FROM unnest($1::text[], $2::timestamptz[], $3::text[], $4::text[], $5::bigint[])
		      AS d(code, bucket, referrer, device, n)
		 JOIN links l ON l.code = d.code
		 ON CONFLICT (code, bucket, referrer, device)
		 DO UPDATE SET clicks = click_stats.clicks + EXCLUDED.clicks`,
		codes, buckets, refs, devs, counts,
	)
	if err != nil {
		return fmt.Errorf("add stats: %w", err)
	}
	return nil
}

// Stats summarizes one link's clicks since the given time. It returns
// ErrNotFound for an unknown code. Slices are never nil, so the JSON shows
// [] and not null.
func (s *PostgresStore) Stats(ctx context.Context, code string, since time.Time) (Stats, error) {
	if _, err := s.Resolve(ctx, code); err != nil {
		return Stats{}, err // includes ErrNotFound
	}

	st := Stats{
		Code:         code,
		Since:        since,
		ByHour:       []HourCount{},
		TopReferrers: []NameCount{},
		Devices:      []NameCount{},
	}

	// SUM(bigint) is numeric in Postgres; casting keeps the scan simple.
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(clicks), 0)::bigint FROM click_stats WHERE code = $1 AND bucket >= $2`,
		code, since,
	).Scan(&st.Total)
	if err != nil {
		return Stats{}, fmt.Errorf("stats total: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT bucket, SUM(clicks)::bigint FROM click_stats
		 WHERE code = $1 AND bucket >= $2 GROUP BY bucket ORDER BY bucket`,
		code, since,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("stats by hour: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h HourCount
		if err := rows.Scan(&h.Hour, &h.Clicks); err != nil {
			return Stats{}, fmt.Errorf("stats by hour: %w", err)
		}
		st.ByHour = append(st.ByHour, h)
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("stats by hour: %w", err)
	}

	if st.TopReferrers, err = s.nameCounts(ctx, "referrer", code, since, 10); err != nil {
		return Stats{}, err
	}
	if st.Devices, err = s.nameCounts(ctx, "device", code, since, 10); err != nil {
		return Stats{}, err
	}
	return st, nil
}

// nameCounts groups one link's clicks by a column. col is spliced into the
// SQL text, so it is checked against an allowlist: never pass user input.
func (s *PostgresStore) nameCounts(ctx context.Context, col, code string, since time.Time, limit int) ([]NameCount, error) {
	if col != "referrer" && col != "device" {
		return nil, fmt.Errorf("stats: unsupported column %q", col)
	}

	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(`SELECT %[1]s, SUM(clicks)::bigint AS c FROM click_stats
		             WHERE code = $1 AND bucket >= $2
		             GROUP BY %[1]s ORDER BY c DESC, %[1]s LIMIT $3`, col),
		code, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("stats by %s: %w", col, err)
	}
	defer rows.Close()

	out := []NameCount{}
	for rows.Next() {
		var nc NameCount
		if err := rows.Scan(&nc.Name, &nc.Clicks); err != nil {
			return nil, fmt.Errorf("stats by %s: %w", col, err)
		}
		if nc.Name == "" {
			nc.Name = "(direct)"
		}
		out = append(out, nc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats by %s: %w", col, err)
	}
	return out, nil
}
