package queries

import (
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/results"
)

func TestPercentileFromHistogram(t *testing.T) {
	hist := func(counts map[int]int64) []int64 {
		h := make([]int64, results.LatencyBuckets)
		for i, c := range counts {
			h[i] = c
		}
		return h
	}
	cases := []struct {
		name string
		hist []int64
		q    float64
		want float64
		ok   bool
	}{
		{"empty", hist(nil), 0.95, 0, false},
		{"nil", nil, 0.95, 0, false},
		{"bad quantile", hist(map[int]int64{0: 1}), 0, 0, false},
		{"single bucket", hist(map[int]int64{3: 10}), 0.95, 100, true},
		// 95 fast (<=50 ms) and 5 slow (<=1000 ms): rank 95 is still in the 50 ms bucket.
		{"rank on the boundary", hist(map[int]int64{2: 95, 6: 5}), 0.95, 50, true},
		// 94 fast and 6 slow: rank 95 falls into the slow bucket.
		{"rank just past the boundary", hist(map[int]int64{2: 94, 6: 6}), 0.95, 1000, true},
		{"median", hist(map[int]int64{0: 1, 1: 1, 4: 2}), 0.5, 25, true},
		{"max", hist(map[int]int64{0: 3, 9: 1}), 1, 10000, true},
		{"overflow reports the last edge", hist(map[int]int64{10: 4}), 0.95, 10000, true},
		{"negative counts ignored", hist(map[int]int64{0: -5, 5: 2}), 0.95, 500, true},
	}
	for _, c := range cases {
		got, ok := PercentileFromHistogram(c.hist, c.q)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestSeriesFillsGaps(t *testing.T) {
	to := time.Date(2026, 10, 20, 12, 7, 0, 0, time.UTC)
	from := to.Add(-time.Hour)
	b := 15 * time.Minute
	buckets := map[int64]*agg{
		bucketKey(from, b):                     {total: 4, ok: 3, latSum: 300, latCount: 3},
		bucketKey(to, b):                       {total: 2, ok: 0},
		bucketKey(from.Add(30*time.Minute), b): {total: 1, ok: 1, latSum: 10, latCount: 1},
	}
	s := series(from, to, b, buckets)
	if len(s) != 5 {
		t.Fatalf("len = %d, want 5", len(s))
	}
	if !s[0].T.Equal(time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)) || !s[4].T.Equal(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("bucket starts %v .. %v", s[0].T, s[4].T)
	}
	if *s[0].OKRatio != 0.75 || *s[0].AvgMs != 100 {
		t.Errorf("first bucket = %v %v", *s[0].OKRatio, *s[0].AvgMs)
	}
	if s[1].OKRatio != nil || s[1].AvgMs != nil || s[3].OKRatio != nil {
		t.Error("empty buckets must be null")
	}
	if *s[2].OKRatio != 1 || *s[2].AvgMs != 10 {
		t.Errorf("third bucket = %v %v", *s[2].OKRatio, *s[2].AvgMs)
	}
	if *s[4].OKRatio != 0 || s[4].AvgMs != nil {
		t.Error("a bucket with only failed checks without response has okRatio 0 and no average")
	}
	for r, n := range map[string]int{Range24h: 97, Range7d: 169, Range30d: 121} {
		spec := rangeSpec[r]
		now := time.Date(2026, 10, 20, 12, 7, 0, 0, time.UTC)
		if got := len(series(now.Add(-spec.span), now, spec.bucket, nil)); got != n || got > 200 {
			t.Errorf("%s: %d points, want %d", r, got, n)
		}
	}
}
