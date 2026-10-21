package notify

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const publicURL = "https://maiak.example.com"

var monitorID = uuid.MustParse("01920000-0000-7000-8000-0000000000aa")

func TestFormatOpened(t *testing.T) {
	started := time.Date(2026, 10, 20, 9, 5, 7, 0, time.FixedZone("x", 3*3600))
	got, err := Format(DeliveryOpened, mustJSON(t, map[string]any{
		"monitorId": monitorID, "monitorName": "API", "url": "https://api.example.com/health",
		"incidentId": uuid.New(), "reason": "timeout", "startedAt": started,
	}), publicURL)
	if err != nil {
		t.Fatal(err)
	}
	want := ":red_circle: *API* is down\nhttps://api.example.com/health\nReason: timeout\nSince: 2026-10-20 06:05:07 UTC\n" +
		"<https://maiak.example.com/monitors/01920000-0000-7000-8000-0000000000aa|Open in Maiak>"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// A heartbeat has no URL; its reason is explained (ADR-0094).
func TestFormatOpenedHeartbeatMissed(t *testing.T) {
	started := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	got, err := Format(DeliveryOpened, mustJSON(t, map[string]any{
		"monitorId": monitorID, "monitorName": "Alertmanager", "url": "",
		"incidentId": uuid.New(), "reason": "heartbeat_missed", "startedAt": started,
	}), publicURL)
	if err != nil {
		t.Fatal(err)
	}
	want := ":red_circle: *Alertmanager* is down\nReason: heartbeat_missed (no ping within the expected interval and grace)\n" +
		"Since: 2026-10-21 08:00:00 UTC\n<https://maiak.example.com/monitors/01920000-0000-7000-8000-0000000000aa|Open in Maiak>"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFormatResolved(t *testing.T) {
	got, err := Format(DeliveryResolved, mustJSON(t, map[string]any{
		"monitorId": monitorID, "monitorName": "API", "durationSeconds": 3725,
	}), publicURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := ":large_green_circle: *API* is back up after 1h 2m"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFormatTLS(t *testing.T) {
	na := time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)
	got, err := Format(DeliveryTLSExpiring, mustJSON(t, map[string]any{
		"monitorId": monitorID, "monitorName": "API", "tlsNotAfter": na, "daysLeft": 13,
	}), publicURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := ":warning: TLS certificate for *API* expires in 13 days (2026-11-02)"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	one := 1
	if got := FormatTLSExpiring(Payload{MonitorName: "A", TLSNotAfter: &na, DaysLeft: &one}); got != ":warning: TLS certificate for *A* expires in 1 day (2026-11-02)" {
		t.Fatalf("one day: %q", got)
	}
}

func TestFormatEscapesControlCharacters(t *testing.T) {
	p := Payload{MonitorID: monitorID, MonitorName: "<!channel> & <https://evil|click>", URL: "https://a.example/a&b/x?a=1&b=<2>", Reason: "a<b"}
	got := FormatOpened(p, publicURL)
	want := ":red_circle: *&lt;!channel&gt; &amp; &lt;https://evil|click&gt;* is down\nhttps://a.example/a&amp;b/x\nReason: a&lt;b\n" +
		"<https://maiak.example.com/monitors/" + monitorID.String() + "|Open in Maiak>"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := FormatResolved(Payload{MonitorName: "a>b"}); got != ":large_green_circle: *a&gt;b* is back up after 0s" {
		t.Fatalf("resolved: %q", got)
	}
	if got := Escape("&amp;"); got != "&amp;amp;" {
		t.Fatalf("double escape: %q", got)
	}
}

func TestDisplayURLDropsQueryAndFragment(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com/health":                     "https://api.example.com/health",
		"https://api.example.com/health?token=s3cr3t&x=1":    "https://api.example.com/health",
		"https://api.example.com/health#access_token=s3cr3t": "https://api.example.com/health",
		"https://api.example.com/v1/a%2Fb?key=s3cr3t#frag":   "https://api.example.com/v1/a%2Fb",
		"https://api.example.com?key=s3cr3t":                 "https://api.example.com",
		"https://api.example.com/?":                          "https://api.example.com/",
		"https://api.example.com:443/p?q":                    "https://api.example.com:443/p",
		"https://user:pass@api.example.com/p":                "https://api.example.com/p",
		"":                                                   "",
		"not a url":                                          "",
		"https://api.example.com/%zz?key=s3cr3t":             "",
	}
	for in, want := range cases {
		if got := DisplayURL(in); got != want {
			t.Errorf("DisplayURL(%q) = %q, want %q", in, got, want)
		}
	}
	got := FormatOpened(Payload{MonitorID: monitorID, MonitorName: "API", URL: "https://api.example.com/h?apikey=s3cr3t#f"}, publicURL)
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "#f") || !strings.Contains(got, "\nhttps://api.example.com/h\n") {
		t.Fatalf("opened message shows the query or fragment: %q", got)
	}
}

func TestFormatRejectsBadPayloads(t *testing.T) {
	for name, c := range map[string]struct {
		kind string
		raw  string
	}{
		"not json":         {DeliveryOpened, "nope"},
		"unknown kind":     {"reminder", `{"monitorName":"a"}`},
		"tls without date": {DeliveryTLSExpiring, `{"monitorName":"a","daysLeft":3}`},
	} {
		if _, err := Format(c.kind, []byte(c.raw), publicURL); !errors.Is(err, ErrBadPayload) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                  "0s",
		-time.Second:                       "0s",
		45 * time.Second:                   "45s",
		time.Minute:                        "1m",
		90 * time.Second:                   "1m",
		2*time.Hour + 5*time.Minute:        "2h 5m",
		3 * time.Hour:                      "3h",
		26*time.Hour + 30*time.Second:      "1d 2h",
		49*time.Hour + 1*time.Minute + 1e9: "2d 1h 1m",
	} {
		if got := HumanDuration(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}

func TestTestMessage(t *testing.T) {
	if got := TestMessage(publicURL); got != ":white_check_mark: Test message from Maiak: alerts will be posted here. <https://maiak.example.com|Open Maiak>" {
		t.Fatalf("got %q", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
