package statuspages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"
)

// SnapshotVersion is the version of the public snapshot format.
const SnapshotVersion = 1

// HistoryDays is the number of daily bars per component (ADR-0103).
const HistoryDays = 90

// Status is the public state of a component, a day or the whole page.
type Status string

// Public states. Component and day states are Operational, Degraded, Outage and NoData; the page
// uses Operational, Degraded, PartialOutage, MajorOutage, or NoData when no component has data.
const (
	Operational   Status = "operational"
	Degraded      Status = "degraded"
	Outage        Status = "outage"
	NoData        Status = "no_data"
	PartialOutage Status = "partial_outage"
	MajorOutage   Status = "major_outage"
)

// Day thresholds: a day is operational at 99.9 % uptime or more, degraded at 99 % or more (about
// 14 minutes of failures), an outage below.
const (
	dayOperationalAt = 0.999
	dayDegradedAt    = 0.99
)

// Snapshot is the public projection of a status page: the allow-list of ADR-0103. It is the only
// shape that is ever stored in status_page_snapshots and served by cmd/statuspage; adding a field
// here publishes it (R-211), and TestSnapshotGoldenKeys fails until the golden key list agrees.
type Snapshot struct {
	Version         int         `json:"version"`
	Page            PublicPage  `json:"page"`
	Status          Status      `json:"status"`
	GeneratedAt     time.Time   `json:"generatedAt"`
	Components      []Component `json:"components"`
	ActiveIncidents []Incident  `json:"activeIncidents"`
	PastIncidents   []Incident  `json:"pastIncidents"`
}

// PublicPage is the branding of the page.
type PublicPage struct {
	Slug        string  `json:"slug"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	LogoURL     *string `json:"logoUrl"`
	AccentColor *string `json:"accentColor"`
}

// Component is one public component: never the monitor's name, URL, project or error kind.
type Component struct {
	Name   string  `json:"name"`
	Group  *string `json:"group"`
	Status Status  `json:"status"`
	// Uptime is the 90-day uptime in percent, nil without data.
	Uptime *float64 `json:"uptime"`
	Days   []Day    `json:"days"`
}

// Day is one daily bar (UTC).
type Day struct {
	Date   string   `json:"date"`
	Status Status   `json:"status"`
	Uptime *float64 `json:"uptime"`
}

// Incident is an automatic incident with a generated public title.
type Incident struct {
	// ID is an opaque reference (a hash), stable per incident, for RSS guids and anchors.
	ID              string     `json:"id"`
	Title           string     `json:"title"`
	Components      []string   `json:"components"`
	StartedAt       time.Time  `json:"startedAt"`
	ResolvedAt      *time.Time `json:"resolvedAt"`
	DurationSeconds int64      `json:"durationSeconds"`
}

// Encode returns the JSON of s and its ETag (32 hex characters of its SHA-256).
func (s Snapshot) Encode() ([]byte, string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:16]), nil
}

// MonitorState is what the builder knows about a component's monitor right now.
type MonitorState struct {
	Paused bool
	// Status is monitor_state.status: unknown, up or down.
	Status string
	// FailingLocations counts locations with failed checks below the failure threshold, DownLocations
	// locations that are down; Locations all selected locations.
	FailingLocations int
	DownLocations    int
	Locations        int
}

// ComponentStatus applies the component rule of ADR-0103: outage when the monitor is down,
// degraded when it is up but some checks fail below the threshold or some locations are down, no
// data when it is paused or has no result yet.
func ComponentStatus(m MonitorState) Status {
	if m.Paused {
		return NoData
	}
	switch m.Status {
	case "down":
		return Outage
	case "up":
		if m.FailingLocations > 0 || m.DownLocations > 0 {
			return Degraded
		}
		return Operational
	}
	return NoData
}

// OverallStatus applies the page rule of ADR-0103 to the component states; components without data
// do not count. NoData when none has data.
func OverallStatus(components []Status) Status {
	n, outages, degraded := 0, 0, 0
	for _, s := range components {
		switch s {
		case Outage:
			outages++
		case Degraded:
			degraded++
		case Operational:
		default:
			continue
		}
		n++
	}
	switch {
	case n == 0:
		return NoData
	case outages == n:
		return MajorOutage
	case outages > 0:
		return PartialOutage
	case degraded > 0:
		return Degraded
	}
	return Operational
}

// DayStatus maps an uptime ratio (0-1) to a bar colour; nil is no data.
func DayStatus(ratio *float64) Status {
	switch {
	case ratio == nil:
		return NoData
	case *ratio >= dayOperationalAt:
		return Operational
	case *ratio >= dayDegradedAt:
		return Degraded
	}
	return Outage
}

// percent turns a ratio into a percentage rounded to three decimals (99.987).
func percent(ratio float64) *float64 {
	v := math.Round(ratio*100*1000) / 1000
	return &v
}

// Counts are checks of one day: total and successful.
type Counts struct {
	Total int64
	OK    int64
}

// CheckDays builds the bars of an HTTP monitor from its daily rollup counts (keyed by UTC date
// "2006-01-02"), oldest first, and the uptime over all of them.
func CheckDays(days []time.Time, counts map[string]Counts) ([]Day, *float64) {
	out := make([]Day, len(days))
	var total, ok int64
	for i, d := range days {
		key := d.Format(time.DateOnly)
		out[i] = Day{Date: key, Status: NoData}
		c, found := counts[key]
		if !found || c.Total <= 0 {
			continue
		}
		total += c.Total
		ok += c.OK
		r := float64(c.OK) / float64(c.Total)
		out[i].Uptime = percent(r)
		out[i].Status = DayStatus(&r)
	}
	if total == 0 {
		return out, nil
	}
	return out, percent(float64(ok) / float64(total))
}

// Interval is a time span; End nil means it is still going on.
type Interval struct {
	Start time.Time
	End   *time.Time
}

// HeartbeatDays builds the bars of a heartbeat from its incident durations (L-275): the observed
// part of each day is from the monitor's creation to now, and down time is the overlap with
// incidents. Days before the creation have no data.
func HeartbeatDays(days []time.Time, created, now time.Time, incidents []Interval) ([]Day, *float64) {
	out := make([]Day, len(days))
	var observedAll, downAll time.Duration
	for i, d := range days {
		start, end := d, d.Add(24*time.Hour)
		out[i] = Day{Date: d.Format(time.DateOnly), Status: NoData}
		if created.After(start) {
			start = created
		}
		if now.Before(end) {
			end = now
		}
		if !end.After(start) {
			continue
		}
		observed := end.Sub(start)
		var down time.Duration
		for _, inc := range overlapping(incidents, now) {
			s, e := inc.Start, *inc.End
			if s.Before(start) {
				s = start
			}
			if e.After(end) {
				e = end
			}
			if e.After(s) {
				down += e.Sub(s)
			}
		}
		down = min(down, observed)
		observedAll += observed
		downAll += down
		r := 1 - float64(down)/float64(observed)
		out[i].Uptime = percent(r)
		out[i].Status = DayStatus(&r)
	}
	if observedAll == 0 {
		return out, nil
	}
	return out, percent(1 - float64(downAll)/float64(observedAll))
}

// overlapping closes open intervals at now and merges overlaps, so down time is never counted twice.
func overlapping(in []Interval, now time.Time) []Interval {
	list := make([]Interval, 0, len(in))
	for _, iv := range in {
		end := now
		if iv.End != nil {
			end = *iv.End
		}
		list = append(list, Interval{Start: iv.Start, End: &end})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
	var out []Interval
	for _, iv := range list {
		if n := len(out); n > 0 && !iv.Start.After(*out[n-1].End) {
			if iv.End.After(*out[n-1].End) {
				out[n-1].End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// HistoryWindow returns the HistoryDays UTC midnights ending today, oldest first.
func HistoryWindow(now time.Time) []time.Time {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	out := make([]time.Time, HistoryDays)
	for i := range out {
		out[i] = today.AddDate(0, 0, i-(HistoryDays-1))
	}
	return out
}

// IncidentTitle is the generated public title of an automatic incident (ADR-0103).
func IncidentTitle(components []string) string {
	switch len(components) {
	case 0:
		return "A service is unavailable"
	case 1:
		return components[0] + " is unavailable"
	}
	return components[0] + " and " + strconv.Itoa(len(components)-1) + " more are unavailable"
}

// IncidentRef is the opaque public reference of an incident: never its id.
func IncidentRef(pageID, incidentID [16]byte) string {
	h := sha256.New()
	h.Write([]byte("maiak-statuspage-incident-v1"))
	h.Write(pageID[:])
	h.Write(incidentID[:])
	return hex.EncodeToString(h.Sum(nil)[:12])
}
