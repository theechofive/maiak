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
// register and getPublicConfig were added deliberately with self-service registration (P-02).
// The heartbeat ping (P-03, ADR-0094) is public on purpose: Alertmanager and cron jobs call it
// without a session, the secret token in its URL is the credential.
var publicOperations = []string{"getHealth", "getPublicConfig", "login", "register", "pingHeartbeat", "pingHeartbeatPost"}

// csrfExempt are the unsafe operations without a session, hence without a CSRF token. The POST
// form of the heartbeat ping is one: it has no session to carry a CSRF token, and it changes only
// the ping time of the heartbeat whose secret token is in the URL.
var csrfExempt = []string{"login", "register", "pingHeartbeatPost"}

func isCSRFExempt(operationID string) bool {
	return slices.ContainsFunc(csrfExempt, func(id string) bool { return strings.EqualFold(id, operationID) })
}

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
	// A well-formed heartbeat token: a malformed one is answered 404 before any other work.
	p = strings.ReplaceAll(p, "{token}", strings.Repeat("A", 43))
	return strings.NewReplacer(
		"{monitorId}", "0192b9c4-7c5e-7000-8000-000000000001",
		"{tenantId}", "0192b9c4-7c5e-7000-8000-000000000002",
		"{userId}", "0192b9c4-7c5e-7000-8000-000000000003",
	).Replace(p)
}

// adminOperations are the platform admin operations (P-04, ADR-0097). Every path under
// /api/v1/admin/ must be one of them, tagged admin and need a session; the integration test
// TestIntegrationAdminOperationsRefuseNonAdmins checks each answers 403 to a normal user.
var adminOperations = []string{"adminGetStats", "adminListTenants", "adminGetTenant", "adminUpdateTenant",
	"adminDeleteTenant", "adminListTenantUsers", "adminResetUserPassword"}

func TestSpecAdminOperations(t *testing.T) {
	spec, ops := specOperations(t)
	seen := 0
	for _, o := range ops {
		isAdminPath := strings.HasPrefix(o.path, adminPathPrefix)
		isAdminOp := slices.ContainsFunc(adminOperations, func(id string) bool { return strings.EqualFold(id, o.id) })
		if isAdminPath != isAdminOp {
			t.Errorf("%s %s (%s): admin path %v but listed as admin operation %v", o.method, o.path, o.id, isAdminPath, isAdminOp)
		}
		if !isAdminPath {
			continue
		}
		seen++
		if !slices.Equal(o.op.Tags, []string{"admin"}) {
			t.Errorf("%s %s: tags %v, want [admin]", o.method, o.path, o.op.Tags)
		}
		security := spec.Security
		if o.op.Security != nil {
			security = *o.op.Security
		}
		if len(security) == 0 {
			t.Errorf("%s %s: no security requirement", o.method, o.path)
		}
		if _, ok := o.op.Responses.Map()["403"]; !ok {
			t.Errorf("%s %s: 403 not documented", o.method, o.path)
		}
	}
	if seen != len(adminOperations) {
		t.Fatalf("%d admin operations in the spec, want %d", seen, len(adminOperations))
	}
}

// Without a session every admin operation is 401, before the platform-admin check.
func TestAdminOperationsNeedSession(t *testing.T) {
	h := newTestHandler(t)
	_, ops := specOperations(t)
	for _, o := range ops {
		if !strings.HasPrefix(o.path, adminPathPrefix) {
			continue
		}
		hdr := map[string]string{"X-CSRF-Token": "x", "Content-Type": "application/json"}
		body := ""
		if o.method == http.MethodPatch {
			body = `{"status":"suspended"}`
		}
		rec, p := do(t, h, o.method, concretePath(o.path), body, hdr)
		if rec.Code != http.StatusUnauthorized || p.Type != problemUnauthenticated {
			t.Errorf("%s %s: code=%d problem=%+v, want 401", o.method, o.path, rec.Code, p)
		}
	}
}

func TestSpecValidates(t *testing.T) {
	spec, _ := specOperations(t)
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatalf("api/openapi.yaml is not a valid OpenAPI document: %v", err)
	}
}

// Every unsafe operation except login and registration must declare the CSRF token, both as a security
// requirement (enforced by the validator) and as a required header parameter (documented for
// the generated client).
func TestSpecUnsafeOperationsRequireCSRF(t *testing.T) {
	spec, ops := specOperations(t)
	for _, o := range ops {
		if !isUnsafe(o.method) || isCSRFExempt(o.id) {
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
