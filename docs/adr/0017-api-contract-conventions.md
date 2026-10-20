# ADR-0017: API contract conventions for the MVP

- Status: Accepted
- Date: 2026-10-20

## Context
Task B-05 extended `api/openapi.yaml` from the slice-1 auth endpoints to the whole MVP contract. Several small choices shape every later handler and the generated TypeScript client, and one of them changes the logout operation in a way `oasdiff` reports as breaking.

## Decision
- **Strict server.** oapi-codegen generates the strict server (typed request and response objects). Handlers return `(response, error)`; every error goes through `writeError` in `internal/api/errors.go`, the single place where errors become problem+json. The handler gets the `*http.Request` (client IP, cookies) through a strict middleware that puts it in the context.
- **Operations without a handler answer 501.** `internal/api/unimplemented.go` is a hand-written type that lists every not-yet-implemented operation explicitly and is embedded in the server; `var _ gen.StrictServerInterface = (*server)(nil)` makes a new operation in the spec a compile error until it is listed there or implemented. A task that implements an operation deletes its stub.
- **400 versus 422.** The request validator rejects anything that does not match the contract (malformed JSON, wrong types, unknown or missing fields, schema limits) with 400. Business rules checked by the domain (URL policy, immutable interval and timeout, Slack URL shape) answer 422. Both carry RFC 9457 `fieldErrors` (`[{field, message}]`) when the failure belongs to a body field. Domain packages report them with `internal/platform/validation.Error`.
- **CSRF in the contract.** Every unsafe operation except login has the security requirement `cookieAuth + csrfToken` and also declares the required `X-CSRF-Token` header parameter, so the generated client exposes it. A test iterates the spec to enforce both, and an integration test checks that each such operation answers 403 without the token.
- **Field names.** New resources (monitors, checks, incidents, settings) use camelCase. The auth resources (`csrf_token`, `expires_at`, `display_name`) and `request_id` in problems keep snake_case: renaming them is a breaking change with no benefit before a public API exists (L-028 tracks the clean-up).
- **`expectedStatus` is a string**: `"2xx"` or a three-digit code such as `"204"`, so the field has one JSON type instead of a `oneOf` that generators handle poorly.
- **Interval and timeout** are plain integers in the schema; the MVP values (60 s, 10 s) are enforced by the domain with 422, so a later relaxation is not a contract change.

## Consequences
- Adding the required header parameter to `logout` is reported by `oasdiff breaking` (new required request parameter) although the behaviour is unchanged: the security requirement already demanded the header. Accepted once, here (R-014).
- Handlers stay small and cannot forget error mapping; tests can call `writeError` directly.
- Clients must read `fieldErrors` from both 400 and 422.

## Alternatives considered
- Keeping the non-strict `ServerInterface` with `gen.Unimplemented`: the generated stub silently covers a forgotten operation, so a missing handler is not a compile error.
- Mapping all body schema failures to 422: would change the login error contract from slice 1.
- `expectedStatus` as `oneOf: [string, integer]`: awkward generated types in Go and TypeScript.
