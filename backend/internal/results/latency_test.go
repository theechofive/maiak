package results

import "testing"

func TestBucket(t *testing.T) {
	cases := map[int]int{0: 0, 10: 0, 11: 1, 25: 1, 26: 2, 100: 3, 101: 4, 999: 6, 1000: 6, 10000: 9, 10001: 10, 1 << 30: 10}
	for ms, want := range cases {
		if got := Bucket(ms); got != want {
			t.Errorf("Bucket(%d) = %d, want %d", ms, got, want)
		}
	}
	if LatencyBuckets != 11 {
		t.Fatalf("LatencyBuckets = %d, want 11 (the SQL CHECK on cardinality)", LatencyBuckets)
	}
}
