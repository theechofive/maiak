package notify

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Channel events (notification_channels.events, ADR-0100).
const (
	EventDown        = "down"
	EventRecovered   = "recovered"
	EventTLSExpiring = "tls_expiring"
)

// AllEvents is the default of a new channel, in display order.
var AllEvents = []string{EventDown, EventRecovered, EventTLSExpiring}

// Channel limits (the database enforces them too).
const (
	MaxChannelNameRunes = 60
	MaxMentions         = 3
	MinReminderMinutes  = 5
	MaxReminderMinutes  = 1440
	// MaxMonitorChannels is how many channels one monitor may select.
	MaxMonitorChannels = 10
)

// mentionPattern accepts exactly the Slack mention forms Maiak sends: @here, @channel, a user
// group <!subteam^ID> and a user <@UID>. Anything else could inject links or formatting.
var mentionPattern = regexp.MustCompile(`^(@here|@channel|<!subteam\^[A-Z0-9]{2,20}>|<@[UW][A-Z0-9]{2,20}>)$`)

// ValidateChannelName trims a channel name and checks it, returning a message for the user.
func ValidateChannelName(raw string) (string, string) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", "is required"
	}
	if utf8.RuneCountInString(name) > MaxChannelNameRunes {
		return "", "must be at most 60 characters"
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", "must not contain control or invisible formatting characters"
		}
	}
	return name, ""
}

// ValidateEvents checks a channel's event list and returns it deduplicated in the order of
// AllEvents.
func ValidateEvents(events []string) ([]string, string) {
	if len(events) == 0 {
		return nil, "must name at least one event"
	}
	for _, e := range events {
		if !slices.Contains(AllEvents, e) {
			return nil, `must be "down", "recovered" or "tls_expiring"`
		}
	}
	var out []string
	for _, e := range AllEvents {
		if slices.Contains(events, e) {
			out = append(out, e)
		}
	}
	return out, ""
}

// ValidateMention checks the mention tokens of a channel and returns them deduplicated.
func ValidateMention(tokens []string) ([]string, string) {
	out := []string{}
	for _, t := range tokens {
		if !mentionPattern.MatchString(t) {
			return nil, "each mention must be @here, @channel, <!subteam^ID> or <@UID>"
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if len(out) > MaxMentions {
		return nil, "must have at most 3 mentions"
	}
	return out, ""
}

// ValidateReminderMinutes accepts 0 (no reminders) or 5..1440.
func ValidateReminderMinutes(n int) string {
	if n != 0 && (n < MinReminderMinutes || n > MaxReminderMinutes) {
		return "must be 0 (off) or between 5 and 1440 minutes"
	}
	return ""
}

// slackMention renders a stored mention token as Slack mrkdwn; it returns "" for a token that
// does not pass validation (defence in depth: stored values were validated on write).
func slackMention(token string) string {
	if !mentionPattern.MatchString(token) {
		return ""
	}
	switch token {
	case "@here":
		return "<!here>"
	case "@channel":
		return "<!channel>"
	}
	return token
}
