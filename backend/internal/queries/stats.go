package queries

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/results"
)

// Stats ranges.
const (
	Range24h = "24h"
	Range7d  = "7d"
	Range30d = "30d"
)

// Point is one bucket of the latency series. AvgMs is nil when no check got a response in the
// bucket, OKRatio when no check ran.
type Point struct {
	T       time.Time
	AvgMs   *float64
	OKRatio *float64
}

// Stats is the response-time summary of a range.
type Stats struct {
	Range  string
	AvgMs  *float64
	P95Ms  *float64
	Series []Point
}

// rangeSpec: 24 h from raw rows in 15-minute buckets (97 points), 7 d from hourly rollups in
// hourly buckets (169), 30 d in 6-hour buckets (121), so a series never exceeds ~200 points.
var rangeSpec = map[string]struct {
	span, bucket time.Duration
}{
	Range24h: {24 * time.Hour, 15 * time.Minute},
	Range7d:  {7 * 24 * time.Hour, time.Hour},
	Range30d: {30 * 24 * time.Hour, 6 * time.Hour},
}

// ValidRange reports whether r is a supported stats range.
func ValidRange(r string) bool {
	_, ok := rangeSpec[r]
	return ok
}

// agg accumulates checks of one bucket. latCount counts checks with a response (latency).
type agg struct {
	total, ok, latSum, latCount int64
}

// Stats returns average and p95 response time and the latency series of a monitor for rng
// (Range24h, Range7d or Range30d). Latency statistics cover checks that got a response, the
// same population as the rollup histogram (ADR-0021).
func (s *Service) Stats(ctx context.Context, tenantID, id uuid.UUID, rng string) (Stats, error) {
	spec, ok := rangeSpec[rng]
	if !ok {
		return Stats{}, fmt.Errorf("queries: unknown stats range %q", rng)
	}
	now := s.clock.Now().UTC()
	out := Stats{Range: rng}
	buckets := map[int64]*agg{}
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := exists(ctx, tx, tenantID, id); err != nil {
			return err
		}
		if rng == Range24h {
			return statsRaw(ctx, tx, id, now.Add(-spec.span), now, spec.bucket, buckets, &out)
		}
		return statsRollups(ctx, tx, id, now.Add(-spec.span).Truncate(time.Hour), now, spec.bucket, buckets, &out)
	})
	if err != nil {
		return Stats{}, wrap("stats", err)
	}
	out.Series = series(now.Add(-spec.span), now, spec.bucket, buckets)
	return out, nil
}

// statsRaw computes the 24 h statistics from raw rows: exact average and p95
// (percentile_cont) and buckets aligned to the bucket size in UTC epoch time.
func statsRaw(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to time.Time, bucket time.Duration, buckets map[int64]*agg, out *Stats) error {
	err := tx.QueryRow(ctx, `
		SELECT avg(latency_ms)::float8, percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms)::float8
		FROM check_results
		WHERE monitor_id = $1 AND checked_at >= $2 AND checked_at < $3 AND latency_ms IS NOT NULL`,
		id, from, to).Scan(&out.AvgMs, &out.P95Ms)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT floor(extract(epoch FROM checked_at) / $4)::bigint, count(*), count(*) FILTER (WHERE ok),
		       COALESCE(sum(latency_ms), 0)::bigint, count(latency_ms)
		FROM check_results
		WHERE monitor_id = $1 AND checked_at >= $2 AND checked_at < $3
		GROUP BY 1`, id, from, to, bucket.Seconds())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k int64
		var a agg
		if err := rows.Scan(&k, &a.total, &a.ok, &a.latSum, &a.latCount); err != nil {
			return err
		}
		buckets[k] = &a
	}
	return rows.Err()
}

// statsRollupsSQL returns per-hour rows from the rollups for the hours that are rolled up and,
// for the hours after the newest rollup (the current hour and any hour jobs has not rolled up
// yet), from raw rows grouped by hour and histogram bucket. width_bucket with thresholds
// edge+1 maps a latency to the index of the bucket whose inclusive upper edge it does not
// exceed, the same buckets as compute_rollups; rows without a latency get a NULL bucket.
const statsRollupsSQL = `
WITH r AS (
    SELECT COALESCE(max(hour) + interval '1 hour', $2::timestamptz) AS raw_from
    FROM result_rollups_hourly
    WHERE monitor_id = $1 AND hour >= $2 AND hour < $3
)
SELECT h.hour, h.total::bigint, h.ok_count::bigint, h.latency_sum_ms, h.latency_histogram, NULL::integer
FROM result_rollups_hourly AS h, r
WHERE h.monitor_id = $1 AND h.hour >= $2 AND h.hour < r.raw_from
UNION ALL
SELECT date_trunc('hour', c.checked_at, 'UTC'), count(*), count(*) FILTER (WHERE c.ok),
       COALESCE(sum(c.latency_ms), 0)::bigint, NULL::integer[], width_bucket(c.latency_ms, $5::integer[])
FROM check_results AS c, r
WHERE c.monitor_id = $1 AND c.checked_at >= r.raw_from AND c.checked_at < $4
GROUP BY 1, 6`

// statsRollups computes 7 d and 30 d statistics from the hourly rollups (plus raw rows for the
// hours not rolled up). p95 comes from the merged histogram (see PercentileFromHistogram).
func statsRollups(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to time.Time, bucket time.Duration, buckets map[int64]*agg, out *Stats) error {
	thresholds := make([]int32, len(results.LatencyBucketEdgesMs))
	for i, e := range results.LatencyBucketEdgesMs {
		thresholds[i] = int32(e + 1) //nolint:gosec // small constants
	}
	rows, err := tx.Query(ctx, statsRollupsSQL, id, from, to.Truncate(time.Hour), to, thresholds)
	if err != nil {
		return err
	}
	defer rows.Close()
	hist := make([]int64, results.LatencyBuckets)
	var latSum int64
	for rows.Next() {
		var hour time.Time
		var total, ok, sum int64
		var h []int32
		var rawBucket *int32
		if err := rows.Scan(&hour, &total, &ok, &sum, &h, &rawBucket); err != nil {
			return err
		}
		k := bucketKey(hour, bucket)
		a := buckets[k]
		if a == nil {
			a = &agg{}
			buckets[k] = a
		}
		a.total += total
		a.ok += ok
		var withLatency int64
		switch {
		case h != nil: // a rollup row
			for i, n := range h {
				if i < len(hist) {
					hist[i] += int64(n)
					withLatency += int64(n)
				}
			}
		case rawBucket != nil: // raw rows with a latency in one bucket
			hist[*rawBucket] += total
			withLatency = total
		}
		if withLatency > 0 {
			a.latSum += sum
			a.latCount += withLatency
			latSum += sum
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var n int64
	for _, c := range hist {
		n += c
	}
	if n > 0 {
		avg := float64(latSum) / float64(n)
		out.AvgMs = &avg
	}
	if p, ok := PercentileFromHistogram(hist, 0.95); ok {
		out.P95Ms = &p
	}
	return nil
}

func bucketKey(t time.Time, bucket time.Duration) int64 {
	return int64(math.Floor(float64(t.Unix()) / bucket.Seconds()))
}

// series lists every bucket from the one containing from to the one containing to, with nil
// values for buckets without checks, so gaps are visible.
func series(from, to time.Time, bucket time.Duration, buckets map[int64]*agg) []Point {
	first, last := bucketKey(from, bucket), bucketKey(to, bucket)
	out := make([]Point, 0, last-first+1)
	for k := first; k <= last; k++ {
		p := Point{T: time.Unix(int64(float64(k)*bucket.Seconds()), 0).UTC()}
		if a := buckets[k]; a != nil {
			p.OKRatio = ratio(a.ok, a.total, 1)
			if a.latCount > 0 {
				v := float64(a.latSum) / float64(a.latCount)
				p.AvgMs = &v
			}
		}
		out = append(out, p)
	}
	return out
}

// PercentileFromHistogram returns an upper bound of the q-quantile (0 < q <= 1) of the
// latencies counted in hist, a histogram over results.LatencyBucketEdgesMs: the inclusive upper
// edge of the bucket that contains the ceil(q*n)-th smallest latency. The true value lies
// between the bucket's lower and upper edge, so this overstates p95 by at most one bucket
// width. For the overflow bucket, which has no upper edge, the last edge is returned (a lower
// bound; checks time out at 10 s, so it is effectively never reached). ok is false for an empty
// histogram.
func PercentileFromHistogram(hist []int64, q float64) (float64, bool) {
	var n int64
	for _, c := range hist {
		n += max(c, 0)
	}
	if n == 0 || q <= 0 || q > 1 {
		return 0, false
	}
	rank := int64(math.Ceil(q * float64(n)))
	var cum int64
	for i, c := range hist {
		cum += max(c, 0)
		if cum >= rank {
			if i < len(results.LatencyBucketEdgesMs) {
				return float64(results.LatencyBucketEdgesMs[i]), true
			}
			break
		}
	}
	return float64(results.LatencyBucketEdgesMs[len(results.LatencyBucketEdgesMs)-1]), true
}
