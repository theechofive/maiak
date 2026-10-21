package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/theechofive/maiak/backend/internal/api"
	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/jobs"
	"github.com/theechofive/maiak/backend/internal/platform/metrics"
	"github.com/theechofive/maiak/backend/internal/worker"
)

// recorder registers into a real registry and keeps every collector, so the descriptors of
// vectors without children (which Gather does not show) can be inspected too.
type recorder struct {
	*prometheus.Registry
	collectors []prometheus.Collector
}

func (r *recorder) Register(c prometheus.Collector) error {
	r.collectors = append(r.collectors, c)
	return r.Registry.Register(c)
}

func (r *recorder) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

// allMetrics registers every metric of api, worker and jobs, as their main.go files do.
func allMetrics(t *testing.T) map[string][]string {
	t.Helper()
	reg := &recorder{Registry: metrics.NewRegistry("test", "v", "c")}
	metrics.NewHTTP(reg)
	if _, err := auth.NewService(nil, auth.Options{Registerer: reg}); err != nil {
		t.Fatal(err)
	}
	worker.NewMetrics(reg)
	jobs.NewMetrics(reg)
	api.NewPingMetrics(reg)

	descRe := regexp.MustCompile(`fqName: "([^"]+)".*constLabels: \{([^}]*)\}, variableLabels: \{([^}]*)\}`)
	nameRe := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)=`)
	out := map[string][]string{}
	for _, c := range reg.collectors {
		ch := make(chan *prometheus.Desc, 16)
		go func() { c.Describe(ch); close(ch) }()
		for d := range ch {
			m := descRe.FindStringSubmatch(d.String())
			if m == nil {
				t.Fatalf("cannot parse %s", d)
			}
			var labels []string
			for _, n := range nameRe.FindAllStringSubmatch(m[2], -1) {
				labels = append(labels, n[1])
			}
			for _, l := range strings.Split(m[3], ",") {
				l = strings.TrimSpace(l)
				if strings.HasPrefix(l, "c(") && strings.HasSuffix(l, ")") { // constrained label
					l = l[2 : len(l)-1]
				}
				if l != "" {
					labels = append(labels, l)
				}
			}
			out[m[1]] = labels
		}
	}
	// Collectors registered inside NewRegistry (build info, Go runtime, process) are seen
	// through Gather.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if _, ok := out[mf.GetName()]; ok {
			continue
		}
		var labels []string
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if !slices.Contains(labels, lp.GetName()) {
					labels = append(labels, lp.GetName())
				}
			}
		}
		out[mf.GetName()] = labels
	}
	return out
}

// No metric may be labelled per tenant, monitor, user or target: cardinality would grow with
// customers and the labels would leak identifiers and URLs to the metrics stack.
func TestNoHighCardinalityOrSecretLabels(t *testing.T) {
	forbidden := []string{"tenant", "monitor", "incident", "user", "email", "url", "host", "path", "webhook", "channel", "session", "token", "ip"}
	all := allMetrics(t)
	if len(all) < 30 {
		t.Fatalf("only %d metrics registered; is a metric set missing?", len(all))
	}
	for name, labels := range all {
		for _, l := range labels {
			lower := strings.ToLower(l)
			for _, f := range forbidden {
				if lower == f || strings.HasPrefix(lower, f+"_") || strings.HasSuffix(lower, "_"+f) {
					t.Errorf("%s has forbidden label %q", name, l)
				}
			}
		}
	}
	// The HTTP metrics label by route template, never by raw path.
	if got := all["maiak_http_requests_total"]; !slices.Equal(got, []string{"route", "method", "code"}) {
		t.Errorf("maiak_http_requests_total labels = %v", got)
	}
}

// The method label is client-chosen; only the standard methods are kept, the rest is "other".
func TestHTTPMethodLabelIsBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := metrics.NewHTTP(reg).Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PROPFIND", "get", "X-RANDOM-1", "X-RANDOM-2"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(m, "/x", nil))
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		counts := map[string]float64{}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "method" {
					counts[l.GetValue()] += m.GetCounter().GetValue() + float64(m.GetHistogram().GetSampleCount())
				}
			}
		}
		want := map[string]float64{"GET": 1, "HEAD": 1, "POST": 1, "PUT": 1, "PATCH": 1, "DELETE": 1, "OPTIONS": 1, "other": 6}
		if len(counts) != len(want) {
			t.Errorf("%s: method labels %v, want %v", mf.GetName(), counts, want)
			continue
		}
		for k, v := range want {
			if counts[k] != v {
				t.Errorf("%s: method %q counted %v, want %v", mf.GetName(), k, counts[k], v)
			}
		}
	}
	if len(mfs) != 2 {
		t.Fatalf("gathered %d families, want the two HTTP metrics", len(mfs))
	}
}

// Every maiak_* metric is listed in the observability runbook.
func TestMetricsAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../../../docs/runbooks/observability.md")
	if err != nil {
		t.Fatal(err)
	}
	for name := range allMetrics(t) {
		if strings.HasPrefix(name, "maiak_") && !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("%s is not documented in docs/runbooks/observability.md", name)
		}
	}
}
