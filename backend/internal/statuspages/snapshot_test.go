package statuspages

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

func TestComponentStatus(t *testing.T) {
	cases := []struct {
		name string
		in   MonitorState
		want Status
	}{
		{"up", MonitorState{Status: "up", Locations: 1}, Operational},
		{"down", MonitorState{Status: "down", Locations: 1, DownLocations: 1}, Outage},
		{"unknown", MonitorState{Status: "unknown"}, NoData},
		{"paused but last up", MonitorState{Status: "up", Paused: true}, NoData},
		{"paused while down", MonitorState{Status: "down", Paused: true}, NoData},
		{"failing below threshold", MonitorState{Status: "up", FailingLocations: 1, Locations: 1}, Degraded},
		{"one of several locations down", MonitorState{Status: "up", DownLocations: 1, Locations: 3}, Degraded},
		{"garbage", MonitorState{Status: "weird"}, NoData},
	}
	for _, c := range cases {
		if got := ComponentStatus(c.in); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestOverallStatus(t *testing.T) {
	cases := []struct {
		in   []Status
		want Status
	}{
		{nil, NoData},
		{[]Status{NoData, NoData}, NoData},
		{[]Status{Operational, Operational}, Operational},
		{[]Status{Operational, NoData}, Operational},
		{[]Status{Operational, Degraded}, Degraded},
		{[]Status{Operational, Outage}, PartialOutage},
		{[]Status{Degraded, Outage}, PartialOutage},
		{[]Status{Outage, Outage}, MajorOutage},
		{[]Status{Outage, NoData}, MajorOutage},
		{[]Status{Outage}, MajorOutage},
	}
	for _, c := range cases {
		if got := OverallStatus(c.in); got != c.want {
			t.Errorf("OverallStatus(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestDayStatus(t *testing.T) {
	for _, c := range []struct {
		in   *float64
		want Status
	}{
		{nil, NoData}, {f64(1), Operational}, {f64(0.999), Operational}, {f64(0.9989), Degraded},
		{f64(0.99), Degraded}, {f64(0.9899), Outage}, {f64(0), Outage},
	} {
		if got := DayStatus(c.in); got != c.want {
			t.Errorf("DayStatus(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestHistoryWindow(t *testing.T) {
	now := time.Date(2026, 11, 6, 23, 30, 0, 0, time.FixedZone("CET", 3600))
	w := HistoryWindow(now)
	if len(w) != 90 || w[89].Format(time.DateOnly) != "2026-11-06" || w[0].Format(time.DateOnly) != "2026-08-09" {
		t.Fatalf("window %s .. %s (%d)", w[0], w[89], len(w))
	}
}

func TestCheckDays(t *testing.T) {
	days := HistoryWindow(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	bars, uptime := CheckDays(days, map[string]Counts{
		"2026-11-05": {Total: 1440, OK: 1440},
		"2026-11-06": {Total: 720, OK: 360},
		"2026-11-04": {Total: 1000, OK: 995},
	})
	if bars[89].Status != Outage || *bars[89].Uptime != 50 || bars[88].Status != Operational || bars[87].Status != Degraded || bars[0].Status != NoData || bars[0].Uptime != nil {
		t.Fatalf("bars: %+v %+v %+v %+v", bars[89], bars[88], bars[87], bars[0])
	}
	if want := 100 * float64(1440+360+995) / float64(1440+720+1000); uptime == nil || *uptime != *percent(want / 100) {
		t.Fatalf("uptime %v, want %v", uptime, want)
	}
	if _, u := CheckDays(days, nil); u != nil {
		t.Fatal("no data must have no uptime")
	}
}

func TestHeartbeatDays(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	days := HistoryWindow(now)
	created := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	bars, uptime := HeartbeatDays(days, created, now, []Interval{
		// 23:00 on the 4th to 01:00 on the 5th: one hour on each day.
		{Start: time.Date(2026, 11, 4, 23, 0, 0, 0, time.UTC), End: &end},
		// Overlapping duplicate: counted once.
		{Start: time.Date(2026, 11, 4, 23, 30, 0, 0, time.UTC), End: &end},
		// Still open since 11:00 today.
		{Start: time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)},
	})
	if bars[86].Status != NoData {
		t.Errorf("day before creation: %+v", bars[86])
	}
	// The 4th was observed for 12 hours, one of them down.
	if bars[87].Uptime == nil || *bars[87].Uptime != *percent(11.0 / 12) || bars[87].Status != Outage {
		t.Errorf("4th: %+v", bars[87])
	}
	if *bars[88].Uptime != *percent(23.0 / 24) {
		t.Errorf("5th: %+v", *bars[88].Uptime)
	}
	if *bars[89].Uptime != *percent(11.0 / 12) {
		t.Errorf("today: %+v", *bars[89].Uptime)
	}
	if want := percent(1 - 3.0/48); *uptime != *want {
		t.Errorf("uptime %v, want %v", *uptime, *want)
	}
}

func TestIncidentTitleAndRef(t *testing.T) {
	if IncidentTitle([]string{"API"}) != "API is unavailable" || IncidentTitle([]string{"API", "Web", "DB"}) != "API and 2 more are unavailable" {
		t.Fatal("titles")
	}
	a := IncidentRef([16]byte{1}, [16]byte{2})
	if len(a) != 24 || a == IncidentRef([16]byte{1}, [16]byte{3}) || a != IncidentRef([16]byte{1}, [16]byte{2}) || strings.Contains(a, "0102") {
		t.Fatalf("ref %s", a)
	}
}

// goldenKeys is every JSON path the public snapshot may contain (ADR-0103, R-211). A new field
// must be added here on purpose, after checking it is safe to publish: never a monitor name, URL,
// project, error kind, latency, tenant or id.
var goldenKeys = []string{
	"activeIncidents[].components[]",
	"activeIncidents[].durationSeconds",
	"activeIncidents[].id",
	"activeIncidents[].resolvedAt",
	"activeIncidents[].startedAt",
	"activeIncidents[].title",
	"components[].days[].date",
	"components[].days[].status",
	"components[].days[].uptime",
	"components[].group",
	"components[].name",
	"components[].status",
	"components[].uptime",
	"generatedAt",
	"page.accentColor",
	"page.description",
	"page.logoUrl",
	"page.slug",
	"page.title",
	"pastIncidents[].components[]",
	"pastIncidents[].durationSeconds",
	"pastIncidents[].id",
	"pastIncidents[].resolvedAt",
	"pastIncidents[].startedAt",
	"pastIncidents[].title",
	"status",
	"version",
}

// JSONPaths lists the leaf paths of a JSON document; arrays become [].
func JSONPaths(t *testing.T, raw []byte) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, c)
			}
		case []any:
			for _, c := range x {
				walk(prefix+"[]", c)
			}
		default:
			set[prefix] = true
		}
	}
	walk("", v)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fullSnapshot fills every field, so every key appears in the JSON.
func fullSnapshot() Snapshot {
	logo, accent, group := "https://cdn.example.com/logo.png", "#1f6feb", "Core"
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	inc := Incident{ID: "abc", Title: "API is unavailable", Components: []string{"API"}, StartedAt: now, ResolvedAt: &now, DurationSeconds: 60}
	return Snapshot{Version: 1, Page: PublicPage{Slug: "acme", Title: "Acme", Description: "d", LogoURL: &logo, AccentColor: &accent},
		Status: Operational, GeneratedAt: now,
		Components:      []Component{{Name: "API", Group: &group, Status: Operational, Uptime: f64(99.9), Days: []Day{{Date: "2026-11-06", Status: Operational, Uptime: f64(100)}}}},
		ActiveIncidents: []Incident{inc}, PastIncidents: []Incident{inc}}
}

func TestSnapshotGoldenKeys(t *testing.T) {
	raw, etag, err := fullSnapshot().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got := JSONPaths(t, raw); !reflect.DeepEqual(got, goldenKeys) {
		t.Fatalf("snapshot keys changed:\n got %v\nwant %v", got, goldenKeys)
	}
	if len(etag) != 32 {
		t.Fatalf("etag %q", etag)
	}
	// No key anywhere may name an internal concept.
	for _, k := range goldenKeys {
		for _, banned := range []string{"monitor", "url\"", "project", "tenant", "error", "latency", "password", "internal"} {
			if strings.Contains(strings.ToLower(k), banned) {
				t.Errorf("key %s contains %q", k, banned)
			}
		}
	}
}

func TestValidateSlug(t *testing.T) {
	for _, ok := range []string{"acme", "a-b", "abc", "status-page-1", strings.Repeat("a", 63)} {
		if msg := ValidateSlug(ok); msg != "" {
			t.Errorf("%q: %s", ok, msg)
		}
	}
	for _, bad := range []string{"", "ab", "-abc", "Acme", "a_b", "a b", "acme/x", "acme.json", strings.Repeat("a", 64), "api", "status", "admin", "embed"} {
		if ValidateSlug(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
	if !slices.IsSorted(ReservedSlugs) {
		t.Error("keep ReservedSlugs sorted (and in sync with the migration)")
	}
	// The migration's CHECK refuses the same words.
	sql, err := os.ReadFile("../../migrations/20261106120000_status_pages.sql")
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(ReservedSlugs))
	for i, w := range ReservedSlugs {
		quoted[i] = "'" + w + "'"
	}
	if !strings.Contains(strings.Join(strings.Fields(string(sql)), " "), strings.Join(quoted, ", ")) {
		t.Error("the reserved slugs of the migration differ from ReservedSlugs")
	}
}

func TestValidateAccent(t *testing.T) {
	if v, msg := ValidateAccent(" #1F6FEB "); msg != "" || v != "#1f6feb" {
		t.Fatalf("%q %q", v, msg)
	}
	for _, bad := range []string{"red", "#fff", "#12345g", "#1234567", "#123456;}body{x:y", "rgb(0,0,0)", "#123456 ", "url(x)"} {
		if _, msg := ValidateAccent(bad + "x"); msg == "" {
			t.Errorf("%q accepted", bad)
		}
		if ValidAccent(bad) {
			t.Errorf("ValidAccent(%q)", bad)
		}
	}
}

func TestValidateLogoURL(t *testing.T) {
	if v, msg := ValidateLogoURL("https://cdn.example.com/a/logo.svg?v=2"); msg != "" || v == "" {
		t.Fatal(msg)
	}
	if o, ok := LogoOrigin("https://cdn.example.com:8443/logo.png"); !ok || o != "https://cdn.example.com:8443" {
		t.Fatalf("origin %q", o)
	}
	for _, bad := range []string{
		"http://cdn.example.com/logo.png", "javascript:alert(1)", "data:image/png;base64,AAAA", "//cdn.example.com/x.png",
		"https://user:pw@cdn.example.com/x.png", "https:///x.png", "https://cdn.example.com/x.png\" onerror=\"alert(1)",
		"https://cdn.example.com/<script>", "https://cdn.example.com/ x.png", "https://cdn.example.com/é.png",
		"https://*.example.com/x.png", "https://a.com;script-src *", "https://" + strings.Repeat("a", 500) + ".com",
	} {
		if _, msg := ValidateLogoURL(bad); msg == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateTextAndPassword(t *testing.T) {
	if v, msg := ValidateText("  API  ", 1, MaxNameLength); msg != "" || v != "API" {
		t.Fatal(v, msg)
	}
	for _, bad := range []string{"", strings.Repeat("x", 61), "a\x00b", "line\nbreak", "API\u202egnp.exe", "Pay\u200bments", "\ufeffAPI", "A\u2066B"} {
		if _, msg := ValidateText(bad, 1, MaxNameLength); msg == "" {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, msg := ValidateText("two\nlines", 0, MaxDescription); msg != "" {
		t.Error("description may have line breaks")
	}
	if _, msg := ValidateText("bidi \u202e override", 0, MaxDescription); msg == "" {
		t.Error("description with a bidi override accepted")
	}
	if _, msg := ValidateText("Zürich – 東京 🚀", 1, MaxNameLength); msg != "" {
		t.Errorf("ordinary non-ASCII refused: %s", msg)
	}
	if ValidatePassword("short") == "" || ValidatePassword(strings.Repeat("x", 129)) == "" || ValidatePassword("twelve chars") != "" {
		t.Fatal("password rules")
	}
}
