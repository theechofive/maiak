package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/theechofive/maiak/backend/internal/api/gen"
)

// publicOperations need no session (compared case-insensitively: the embedded spec has the
// operation ids as Go names). Everything else in the contract must answer 401 without one.
var publicOperations = []string{"getHealth", "login"}

type specOperation struct {
	method, path, id string
	op               *openapi3.Operation
}

func specOperations(t *testing.T) (*openapi3.T, []specOperation) {
	t.Helper()
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	var ops []specOperation
	for _, path := range spec.Paths.InMatchingOrder() {
		for method, op := range spec.Paths.Value(path).Operations() {
			ops = append(ops, specOperation{method: method, path: path, id: op.OperationID, op: op})
		}
	}
	if len(ops) < 10 {
		t.Fatalf("only %d operations in the spec", len(ops))
	}
	return spec, ops
}

// concretePath fills path parameters with a syntactically valid value.
func concretePath(p string) string {
	return strings.ReplaceAll(p, "{monitorId}", "0192b9c4-7c5e-7000-8000-000000000001")
}

func TestSpecValidates(t *testing.T) {
	spec, _ := specOperations(t)
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatalf("api/openapi.yaml is not a valid OpenAPI document: %v", err)
	}
}

// Every unsafe operation except login must declare the CSRF token, both as a security
// requirement (enforced by the validator) and as a required header parameter (documented for
// the generated client).
func TestSpecUnsafeOperationsRequireCSRF(t *testing.T) {
	spec, ops := specOperations(t)
	for _, o := range ops {
		if !isUnsafe(o.method) || strings.EqualFold(o.id, "login") {
			continue
		}
		security := spec.Security
		if o.op.Security != nil {
			security = *o.op.Security
		}
		if len(security) == 0 {
			t.Errorf("%s %s (%s): no security requirement", o.method, o.path, o.id)
		}
		for _, req := range security {
			_, cookie := req["cookieAuth"]
			_, csrf := req["csrfToken"]
			if !cookie || !csrf {
				t.Errorf("%s %s (%s): security requirement %v lacks cookieAuth+csrfToken", o.method, o.path, o.id, req)
			}
		}
		p := o.op.Parameters.GetByInAndName(openapi3.ParameterInHeader, csrfHeader)
		if p == nil || !p.Required {
			t.Errorf("%s %s (%s): missing required %s header parameter", o.method, o.path, o.id, csrfHeader)
		}
	}
}

func TestEveryOperationExceptPublicNeedsSession(t *testing.T) {
	h := newTestHandler(t)
	_, ops := specOperations(t)
	for _, o := range ops {
		rec, p := do(t, h, o.method, concretePath(o.path), "", nil)
		if slices.ContainsFunc(publicOperations, func(id string) bool { return strings.EqualFold(id, o.id) }) {
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusNotFound {
				t.Errorf("%s %s (%s) is public but answered %d", o.method, o.path, o.id, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusUnauthorized || p.Type != problemUnauthenticated {
			t.Errorf("%s %s (%s): code=%d problem=%+v, want 401", o.method, o.path, o.id, rec.Code, p)
		}
	}
}
