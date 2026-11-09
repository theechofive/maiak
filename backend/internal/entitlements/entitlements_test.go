package entitlements_test

import (
	"testing"

	"github.com/theechofive/maiak/backend/internal/entitlements"
)

func TestIncludes(t *testing.T) {
	features := []string{"audit-export", "sso"}
	for _, c := range []struct {
		feature string
		want    bool
	}{{"sso", true}, {"audit-export", true}, {"scim", false}, {"", false}, {"SSO", false}} {
		if got := entitlements.Includes(features, c.feature); got != c.want {
			t.Errorf("Includes(%q) = %t, want %t", c.feature, got, c.want)
		}
	}
	if entitlements.Includes(nil, "sso") {
		t.Error("Includes(nil) = true")
	}
}
