package plans

import (
	"errors"
	"testing"
)

const freeLimits = `{"max_monitors":3,"max_heartbeats":1,"max_channels":1}`

func TestResolve(t *testing.T) {
	cases := []struct {
		name      string
		overrides string
		want      Limits
	}{
		{"no overrides", `{}`, Limits{MaxMonitors: 3, MaxHeartbeats: 1, MaxChannels: 1}},
		{"empty column value", ``, Limits{MaxMonitors: 3, MaxHeartbeats: 1, MaxChannels: 1}},
		// The pre-plans override key keeps working.
		{"max_monitors override", `{"max_monitors":5}`, Limits{MaxMonitors: 5, MaxHeartbeats: 1, MaxChannels: 1}},
		{"all overridden", `{"max_monitors":0,"max_heartbeats":7,"max_channels":2}`, Limits{MaxMonitors: 0, MaxHeartbeats: 7, MaxChannels: 2}},
		{"unknown keys ignored", `{"max_interval":30,"max_channels":4}`, Limits{MaxMonitors: 3, MaxHeartbeats: 1, MaxChannels: 4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve("free", []byte(freeLimits), []byte(c.overrides))
			if err != nil || got != c.want {
				t.Fatalf("Resolve = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

func TestResolveUnknownPlan(t *testing.T) {
	if _, err := Resolve("gold", nil, []byte(`{"max_monitors":5}`)); !errors.Is(err, ErrUnknownPlan) {
		t.Fatalf("err = %v, want ErrUnknownPlan", err)
	}
}

func TestResolveRejectsInvalidValues(t *testing.T) {
	for _, bad := range []string{
		`{"max_monitors":-1}`,
		`{"max_monitors":1.5}`,
		`{"max_monitors":4.0}`,
		`{"max_monitors":"3"}`,
		`{"max_monitors":null}`,
		`{"max_monitors":true}`,
		`{"max_channels":1e20}`,
		`[1,2]`,
		`not json`,
	} {
		if _, err := Resolve("free", []byte(freeLimits), []byte(bad)); !errors.Is(err, ErrInvalidLimits) {
			t.Errorf("overrides %s: err = %v, want ErrInvalidLimits", bad, err)
		}
		if _, err := Resolve("free", []byte(bad), nil); !errors.Is(err, ErrInvalidLimits) {
			t.Errorf("plan limits %s: err = %v, want ErrInvalidLimits", bad, err)
		}
	}
}

func TestResolvePlanMissingKey(t *testing.T) {
	// A key missing from the plan is an error unless the tenant overrides it.
	plan := []byte(`{"max_monitors":3,"max_channels":1}`)
	if _, err := Resolve("partial", plan, nil); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("err = %v, want ErrInvalidLimits", err)
	}
	got, err := Resolve("partial", plan, []byte(`{"max_heartbeats":2}`))
	if err != nil || got != (Limits{MaxMonitors: 3, MaxHeartbeats: 2, MaxChannels: 1}) {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
}

func TestLimitsOf(t *testing.T) {
	l := Limits{MaxMonitors: 1, MaxHeartbeats: 2, MaxChannels: 3}
	if l.Of(ResourceMonitor) != 1 || l.Of(ResourceHeartbeat) != 2 || l.Of(ResourceChannel) != 3 {
		t.Fatalf("Of: %+v", l)
	}
}
