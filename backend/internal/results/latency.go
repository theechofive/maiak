// Package results owns check_results (raw, monthly partitions) and result_rollups_hourly
// (ADR-0005, ADR-0016). The worker writes raw results through this package; rollups are
// computed by the compute_rollups database function and read by internal/queries.
package results

// LatencyBucketEdgesMs are the inclusive upper edges, in milliseconds, of the latency histogram
// buckets in result_rollups_hourly.latency_histogram. A final overflow bucket counts latencies
// above the last edge, so a histogram has LatencyBuckets entries. They must match the edges
// hard-coded in compute_rollups (migration 20261020130000_results.sql); an integration test
// checks that.
var LatencyBucketEdgesMs = [...]int{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// LatencyBuckets is the length of a latency histogram.
const LatencyBuckets = len(LatencyBucketEdgesMs) + 1

// Bucket returns the histogram index for a latency in milliseconds.
func Bucket(ms int) int {
	for i, edge := range LatencyBucketEdgesMs {
		if ms <= edge {
			return i
		}
	}
	return len(LatencyBucketEdgesMs)
}
