package uuidv7

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewIsVersion7AndRFCVariant(t *testing.T) {
	id := New()
	if id.Version() != 7 {
		t.Fatalf("version = %d, want 7", id.Version())
	}
	if id.Variant() != uuid.RFC4122 {
		t.Fatalf("variant = %v, want RFC4122", id.Variant())
	}
}

func TestNewIsStrictlyIncreasing(t *testing.T) {
	prev := New()
	for i := 0; i < 100_000; i++ {
		next := New()
		if bytes.Compare(prev[:], next[:]) >= 0 {
			t.Fatalf("id %d not greater than previous: %s <= %s", i, next, prev)
		}
		prev = next
	}
}

func TestNewEmbedsCurrentTime(t *testing.T) {
	before := time.Now().Add(-time.Second)
	id := New()
	after := time.Now().Add(time.Second)
	sec, nsec := id.Time().UnixTime()
	ts := time.Unix(sec, nsec)
	if ts.Before(before) || ts.After(after) {
		t.Fatalf("embedded time %v outside [%v, %v]", ts, before, after)
	}
}
