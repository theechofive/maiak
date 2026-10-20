package machine

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func at(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }

func ok(i int) Result { return Result{OK: true, CheckedAt: at(i)} }

func fail(i int) Result { return Result{ErrorKind: "timeout", CheckedAt: at(i)} }

func ptr(t time.Time) *time.Time { return &t }

// step is one result fed to the machine and what must come out.
type step struct {
	res        Result
	status     Status // aggregated status after the step
	transition Transition
	emit       []Kind
	ignored    bool
}

// run feeds the steps through Apply for a single-location monitor, carrying state forward.
func run(t *testing.T, cfg Config, steps []step) (MonitorState, LocationState) {
	t.Helper()
	var ms MonitorState
	ms.Status = Unknown
	ls := LocationState{Status: Unknown}
	for i, s := range steps {
		out := Apply(Input{Monitor: ms, Location: ls, Result: s.res, Config: cfg, Now: s.res.CheckedAt})
		if out.Ignored != s.ignored {
			t.Fatalf("step %d: ignored = %t, want %t", i, out.Ignored, s.ignored)
		}
		if out.Monitor.Status != s.status || out.Transition != s.transition || !slices.Equal(out.Emit, s.emit) {
			t.Fatalf("step %d: status %s transition %s emit %v; want %s %s %v", i,
				out.Monitor.Status, out.Transition, out.Emit, s.status, s.transition, s.emit)
		}
		ms, ls = out.Monitor, out.Location
	}
	return ms, ls
}

var notify = Config{FailureThreshold: 2, NotifySlack: true}

func TestThresholds(t *testing.T) {
	opened := []Kind{KindOpened}
	cases := []struct {
		name      string
		threshold int
		steps     []step
	}{
		{"threshold 1 opens on the first failure", 1, []step{
			{res: ok(0), status: Up, transition: None},
			{res: fail(1), status: Down, transition: Opened, emit: opened},
			{res: fail(2), status: Down, transition: None},
		}},
		{"threshold 2 needs two consecutive failures", 2, []step{
			{res: ok(0), status: Up, transition: None},
			{res: fail(1), status: Up, transition: None},
			{res: fail(2), status: Down, transition: Opened, emit: opened},
			{res: fail(3), status: Down, transition: None},
		}},
		{"threshold 3", 3, []step{
			{res: ok(0), status: Up, transition: None},
			{res: fail(1), status: Up, transition: None},
			{res: fail(2), status: Up, transition: None},
			{res: fail(3), status: Down, transition: Opened, emit: opened},
		}},
		{"threshold below 1 behaves as 1", 0, []step{
			{res: fail(0), status: Down, transition: Opened, emit: opened},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run(t, Config{FailureThreshold: c.threshold, NotifySlack: true}, c.steps)
		})
	}
}

func TestFirstEverResult(t *testing.T) {
	t.Run("success: unknown to up without a notification", func(t *testing.T) {
		ms, ls := run(t, notify, []step{{res: ok(0), status: Up, transition: None}})
		if !ms.StatusChangedAt.Equal(at(0)) || !ms.LastCheckedAt.Equal(at(0)) || ls.Status != Up || ls.ConsecutiveFailures != 0 {
			t.Fatalf("monitor %+v location %+v", ms, ls)
		}
	})
	t.Run("failures below the threshold stay unknown", func(t *testing.T) {
		ms, ls := run(t, notify, []step{{res: fail(0), status: Unknown, transition: None}})
		if ls.ConsecutiveFailures != 1 || ls.Status != Unknown || ms.LastErrorKind != "timeout" || !ms.StatusChangedAt.IsZero() {
			t.Fatalf("monitor %+v location %+v", ms, ls)
		}
	})
	t.Run("a monitor that is down from the start opens an incident", func(t *testing.T) {
		ms, _ := run(t, notify, []step{
			{res: fail(0), status: Unknown, transition: None},
			{res: fail(1), status: Down, transition: Opened, emit: []Kind{KindOpened}},
		})
		if !ms.IncidentOpen || !ms.StatusChangedAt.Equal(at(1)) {
			t.Fatalf("monitor %+v", ms)
		}
	})
}

func TestRecoveryAndFlapping(t *testing.T) {
	t.Run("recovery resolves on the first success", func(t *testing.T) {
		ms, ls := run(t, notify, []step{
			{res: ok(0), status: Up, transition: None},
			{res: fail(1), status: Up, transition: None},
			{res: fail(2), status: Down, transition: Opened, emit: []Kind{KindOpened}},
			{res: ok(3), status: Up, transition: Resolved, emit: []Kind{KindResolved}},
		})
		if ms.IncidentOpen || ms.LastErrorKind != "" || !ms.StatusChangedAt.Equal(at(3)) || ls.ConsecutiveFailures != 0 {
			t.Fatalf("monitor %+v location %+v", ms, ls)
		}
	})
	t.Run("alternating results below the threshold never alert", func(t *testing.T) {
		var steps []step
		for i := range 10 {
			if i%2 == 0 {
				steps = append(steps, step{res: ok(i), status: Up, transition: None})
			} else {
				steps = append(steps, step{res: fail(i), status: Up, transition: None})
			}
		}
		run(t, notify, steps)
	})
	t.Run("flapping across the threshold alerts once per down period", func(t *testing.T) {
		run(t, notify, []step{
			{res: fail(0), status: Unknown, transition: None},
			{res: fail(1), status: Down, transition: Opened, emit: []Kind{KindOpened}},
			{res: ok(2), status: Up, transition: Resolved, emit: []Kind{KindResolved}},
			{res: fail(3), status: Up, transition: None},
			{res: ok(4), status: Up, transition: None},
			{res: fail(5), status: Up, transition: None},
			{res: fail(6), status: Down, transition: Opened, emit: []Kind{KindOpened}},
			{res: fail(7), status: Down, transition: None},
		})
	})
	t.Run("notifications off: transitions without outbox kinds", func(t *testing.T) {
		ms, _ := run(t, Config{FailureThreshold: 1}, []step{
			{res: fail(0), status: Down, transition: Opened},
			{res: ok(1), status: Up, transition: Resolved},
		})
		if ms.IncidentOpen {
			t.Fatal("incident still open")
		}
	})
}

func TestOutOfOrderResultsIgnored(t *testing.T) {
	ms, ls := run(t, notify, []step{
		{res: ok(5), status: Up, transition: None},
		{res: fail(3), status: Up, transition: None, ignored: true},
		{res: fail(5), status: Up, transition: None, ignored: true}, // same instant: a duplicate
		{res: fail(6), status: Up, transition: None},
	})
	if ls.ConsecutiveFailures != 1 || !ls.LastCheckedAt.Equal(at(6)) || !ms.LastCheckedAt.Equal(at(6)) {
		t.Fatalf("monitor %+v location %+v", ms, ls)
	}
	// An ignored result changes nothing at all.
	in := Input{Monitor: ms, Location: ls, Result: fail(1), Config: notify, Now: at(7)}
	out := Apply(in)
	if !out.Ignored || out.Monitor != in.Monitor || out.Location != in.Location || out.TLSWarning || len(out.Emit) != 0 {
		t.Fatalf("ignored result changed state: %+v", out)
	}
}

func TestQuorumAcrossLocations(t *testing.T) {
	ms := MonitorState{Status: Unknown}
	a, b := LocationState{Status: Unknown}, LocationState{Status: Unknown}
	apply := func(loc *LocationState, other LocationState, r Result) Output {
		out := Apply(Input{Monitor: ms, Location: *loc, Others: []LocationState{other}, Result: r, Config: notify, Now: r.CheckedAt})
		ms, *loc = out.Monitor, out.Location
		return out
	}
	check := func(out Output, status Status, tr Transition) {
		t.Helper()
		if out.Monitor.Status != status || out.Transition != tr {
			t.Fatalf("status %s transition %s, want %s %s", out.Monitor.Status, out.Transition, status, tr)
		}
	}
	// A reports down while B has never reported: not all locations are down.
	check(apply(&a, b, fail(0)), Unknown, None)
	check(apply(&a, b, fail(1)), Unknown, None)
	// B is up: aggregated up, no incident although A is down.
	check(apply(&b, a, ok(1)), Up, None)
	check(apply(&a, b, fail(2)), Up, None)
	// B goes down too: all selected locations are down.
	check(apply(&b, a, fail(2)), Up, None)
	out := apply(&b, a, fail(3))
	check(out, Down, Opened)
	if !slices.Equal(out.Emit, []Kind{KindOpened}) {
		t.Fatalf("emit %v", out.Emit)
	}
	// One location recovers: resolved.
	out = apply(&a, b, ok(4))
	check(out, Up, Resolved)
	if !slices.Equal(out.Emit, []Kind{KindResolved}) {
		t.Fatalf("emit %v", out.Emit)
	}
}

func TestTLSWarning(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name    string
		left    time.Duration
		warned  *time.Duration // offset of the already warned certificate's notAfter from now
		notify  bool
		warn    bool
		emitTLS bool
	}{
		{name: "15 days left: no warning", left: 15 * day},
		{name: "14 days and a minute: no warning", left: 14*day + time.Minute},
		{name: "exactly 14 days: warning", left: 14 * day, notify: true, warn: true, emitTLS: true},
		{name: "1 day left: warning", left: day, notify: true, warn: true, emitTLS: true},
		{name: "same certificate already warned", left: 10 * day, warned: durPtr(10 * day), notify: true},
		{name: "renewed certificate expiring again is a new certificate", left: 10 * day, warned: durPtr(-80 * day), notify: true, warn: true, emitTLS: true},
		{name: "notifications off: decided but not emitted", left: 3 * day, warn: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := at(0)
			ms := MonitorState{Status: Up}
			if c.warned != nil {
				ms.TLSWarnedNotAfter = ptr(now.Add(*c.warned))
			}
			notAfter := now.Add(c.left)
			out := Apply(Input{
				Monitor: ms, Location: LocationState{Status: Up},
				Result: Result{OK: true, CheckedAt: now, TLSNotAfter: &notAfter},
				Config: Config{FailureThreshold: 2, NotifySlack: c.notify}, Now: now,
			})
			if out.TLSWarning != c.warn || slices.Contains(out.Emit, KindTLSExpiring) != c.emitTLS {
				t.Fatalf("warning %t emit %v; want warning %t emitted %t", out.TLSWarning, out.Emit, c.warn, c.emitTLS)
			}
			if out.Monitor.TLSExpiresAt == nil || !out.Monitor.TLSExpiresAt.Equal(notAfter) {
				t.Fatalf("tls_expires_at = %v, want %s", out.Monitor.TLSExpiresAt, notAfter)
			}
			switch {
			case c.warn && !out.Monitor.TLSWarnedNotAfter.Equal(notAfter):
				t.Fatalf("tls_warned_not_after = %v, want %s", out.Monitor.TLSWarnedNotAfter, notAfter)
			case !c.warn && c.warned == nil && out.Monitor.TLSWarnedNotAfter != nil:
				t.Fatalf("tls_warned_not_after set without a warning")
			}
		})
	}

	t.Run("renewal after a warning, then the new certificate nears expiry", func(t *testing.T) {
		old := at(0).Add(5 * day)
		ms := MonitorState{Status: Up, TLSWarnedNotAfter: &old}
		renewed := at(0).Add(90 * day)
		out := Apply(Input{Monitor: ms, Location: LocationState{Status: Up},
			Result: Result{OK: true, CheckedAt: at(1), TLSNotAfter: &renewed}, Config: notify, Now: at(1)})
		if out.TLSWarning || !out.Monitor.TLSWarnedNotAfter.Equal(old) {
			t.Fatalf("renewed certificate: warning %t warned %v", out.TLSWarning, out.Monitor.TLSWarnedNotAfter)
		}
		later := renewed.Add(-13 * day)
		out = Apply(Input{Monitor: out.Monitor, Location: out.Location,
			Result: Result{OK: true, CheckedAt: later, TLSNotAfter: &renewed}, Config: notify, Now: later})
		if !out.TLSWarning || !slices.Equal(out.Emit, []Kind{KindTLSExpiring}) {
			t.Fatalf("renewed certificate near expiry: warning %t emit %v", out.TLSWarning, out.Emit)
		}
		// And only once.
		out = Apply(Input{Monitor: out.Monitor, Location: out.Location,
			Result: Result{OK: true, CheckedAt: later.Add(time.Minute), TLSNotAfter: &renewed}, Config: notify, Now: later.Add(time.Minute)})
		if out.TLSWarning || len(out.Emit) != 0 {
			t.Fatalf("second warning for the same certificate: %v", out.Emit)
		}
	})

	t.Run("a failed check with a certificate still warns, after the incident kind", func(t *testing.T) {
		notAfter := at(0).Add(2 * day)
		out := Apply(Input{Monitor: MonitorState{Status: Up}, Location: LocationState{Status: Up},
			Result: Result{ErrorKind: "unexpected_status", CheckedAt: at(0), TLSNotAfter: &notAfter},
			Config: Config{FailureThreshold: 1, NotifySlack: true}, Now: at(0)})
		if !slices.Equal(out.Emit, []Kind{KindOpened, KindTLSExpiring}) {
			t.Fatalf("emit %v", out.Emit)
		}
	})
}

func durPtr(d time.Duration) *time.Duration { return &d }
