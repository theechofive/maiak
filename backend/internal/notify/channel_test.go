package notify

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateChannelName(t *testing.T) {
	if got, msg := ValidateChannelName("  #ops alerts  "); got != "#ops alerts" || msg != "" {
		t.Fatalf("got %q %q", got, msg)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("é", 61), "a\tb", "a\u202eb"} {
		if _, msg := ValidateChannelName(bad); msg == "" {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, msg := ValidateChannelName(strings.Repeat("é", 60)); msg != "" {
		t.Errorf("60 runes rejected: %s", msg)
	}
}

func TestValidateEvents(t *testing.T) {
	got, msg := ValidateEvents([]string{"tls_expiring", "down", "down"})
	if msg != "" || !slices.Equal(got, []string{"down", "tls_expiring"}) {
		t.Fatalf("got %v %q", got, msg)
	}
	for _, bad := range [][]string{nil, {}, {"agent_offline"}, {"down", "DOWN"}} {
		if _, msg := ValidateEvents(bad); msg == "" {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestValidateMention(t *testing.T) {
	good := []string{"@here", "@channel", "<!subteam^S0123ABC>", "<@U0ABC12>", "<@W0ABC12>"}
	for _, g := range good {
		if _, msg := ValidateMention([]string{g}); msg != "" {
			t.Errorf("%q rejected: %s", g, msg)
		}
	}
	if got, msg := ValidateMention([]string{"@here", "@here", "@channel"}); msg != "" || !slices.Equal(got, []string{"@here", "@channel"}) {
		t.Fatalf("dedup: %v %q", got, msg)
	}
	if got, msg := ValidateMention(nil); msg != "" || got == nil || len(got) != 0 {
		t.Fatalf("empty: %#v %q", got, msg)
	}
	for _, bad := range [][]string{
		{"@everyone"}, {"here"}, {"@Here"}, {"<!here>"}, {"<!channel>"}, {"<!subteam^s01>"}, {"<!subteam^S0|x>"},
		{"<@U0ABC|name>"}, {"<@B0ABC>"}, {"<https://evil.example|click>"}, {" @here"}, {"@here "}, {"<@U0ABC>x"},
		{"@here", "@channel", "<@U0ABC>", "<@U0ABD>"},
	} {
		if _, msg := ValidateMention(bad); msg == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateReminderMinutes(t *testing.T) {
	for _, ok := range []int{0, 5, 60, 1440} {
		if msg := ValidateReminderMinutes(ok); msg != "" {
			t.Errorf("%d rejected", ok)
		}
	}
	for _, bad := range []int{-1, 1, 4, 1441} {
		if msg := ValidateReminderMinutes(bad); msg == "" {
			t.Errorf("%d accepted", bad)
		}
	}
}
