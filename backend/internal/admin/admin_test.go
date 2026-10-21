package admin

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/theechofive/maiak/backend/internal/platform/validation"
)

func TestTemporaryPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		p, err := temporaryPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != temporaryPasswordLen || len(p) < 20 {
			t.Fatalf("length %d", len(p))
		}
		for _, c := range p {
			if !strings.ContainsRune(temporaryPasswordAlphabet, c) {
				t.Fatalf("%q has %q outside the alphabet", p, c)
			}
		}
		if seen[p] {
			t.Fatalf("repeated password %q", p)
		}
		seen[p] = true
	}
	if len(temporaryPasswordAlphabet) != 32 {
		t.Fatalf("alphabet has %d characters; 256 must be a multiple of it", len(temporaryPasswordAlphabet))
	}
}

func TestPageNormalized(t *testing.T) {
	for _, c := range []struct{ in, want Page }{
		{Page{}, Page{Limit: DefaultLimit}},
		{Page{Limit: 500, Offset: -3}, Page{Limit: MaxLimit}},
		{Page{Query: "x", Limit: 10, Offset: 20}, Page{Query: "x", Limit: 10, Offset: 20}},
	} {
		if got := c.in.normalized(); got != c.want {
			t.Errorf("%+v.normalized() = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestMapError(t *testing.T) {
	for code, want := range map[string]error{
		"MKA01": ErrForbidden, "MKA02": ErrNotFound, "MKA05": ErrProtectedTenant,
		"MKA06": ErrOwnTenant, "MKA07": ErrSystemAdmin, "MKA08": ErrTenantDeleted,
	} {
		if got := mapError("op", &pgconn.PgError{Code: code}); !errors.Is(got, want) {
			t.Errorf("%s: %v, want %v", code, got, want)
		}
	}
	for code, field := range map[string]string{"MKA03": "planKey", "MKA04": "status"} {
		var verr *validation.Error
		if got := mapError("op", &pgconn.PgError{Code: code}); !errors.As(got, &verr) || verr.Fields[0].Field != field {
			t.Errorf("%s: %v, want a validation error on %s", code, got, field)
		}
	}
	other := errors.New("boom")
	if got := mapError("op", other); !errors.Is(got, other) || !strings.Contains(got.Error(), "admin: op") {
		t.Errorf("other error: %v", got)
	}
}
