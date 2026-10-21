package jobs

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the jobs metrics, served on the admin port.
type Metrics struct {
	deliveries   *prometheus.CounterVec
	duration     prometheus.Histogram
	pending      prometheus.Gauge
	claimErrors  prometheus.Counter
	housekeeping *prometheus.CounterVec
	lastSuccess  *prometheus.GaugeVec
	// partitionsKept counts expired partitions retention could not drop, by reason.
	partitionsKept *prometheus.CounterVec
	// heartbeatsMissed counts missed heartbeat deadlines applied to the incident machine.
	heartbeatsMissed prometheus.Counter
}

// NewMetrics registers the jobs metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_jobs_deliveries_total", Help: "Delivery attempts recorded, by outcome (sent, retry, failed).",
		}, []string{"status"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "maiak_jobs_delivery_duration_seconds", Help: "Duration of one Slack send.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "maiak_jobs_outbox_pending", Help: "Outbox rows waiting to be delivered (including retries), all tenants.",
		}),
		claimErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_jobs_claim_errors_total", Help: "Failed claim_deliveries calls.",
		}),
		housekeeping: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_jobs_housekeeping_runs_total", Help: "Housekeeping task runs by task and result (ok, skipped, error).",
		}, []string{"task", "result"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "maiak_jobs_housekeeping_last_success_timestamp_seconds", Help: "Unix time of the last successful run of each housekeeping task.",
		}, []string{"task"}),
		partitionsKept: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "maiak_jobs_partitions_kept_total", Help: "Expired check_results partitions retention kept back, by reason (rollups_incomplete, error).",
		}, []string{"reason"}),
		heartbeatsMissed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "maiak_jobs_heartbeats_missed_total", Help: "Missed heartbeat deadlines applied as failures (ADR-0094).",
		}),
	}
	for _, s := range []string{statusSent, "retry", statusFailed} {
		m.deliveries.WithLabelValues(s)
	}
	for _, r := range []string{keptRollupsIncomplete, keptError} {
		m.partitionsKept.WithLabelValues(r)
	}
	reg.MustRegister(m.deliveries, m.duration, m.pending, m.claimErrors, m.housekeeping, m.lastSuccess, m.partitionsKept, m.heartbeatsMissed)
	return m
}
