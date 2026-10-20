# ADR-0080: End-to-end test through the real SSRF guard (`maiaktest` build tag)

- Status: Accepted
- Date: 2026-10-20

## Context
The SSRF guard (`internal/safehttp`, ADR-0011, ADR-0016, ADR-0018) refuses loopback and any port other than 443 by design, so before B-16 no test outside `safehttp` ran a check or a Slack delivery through it: the worker and notifier tests used plain clients (L-030, L-041). An end-to-end test needs loopback TLS servers on random ports, but whatever lets it reach them must not exist in a release binary, because a bypass reachable from production code would undo the guard.

## Decision
- `internal/safehttp/testsupport_maiaktest.go` (`//go:build maiaktest`) exports `NewLoopbackTestClient(opts, roots, port)`. It sets two unexported options: loopback addresses (`127.0.0.0/8`, `::1`) pass the address check and validated addresses are dialed on `port` instead of 443; `roots` replaces the system CAs. Everything else stays real: https only, URL port 443, redirect limit and re-validation, all other blocked ranges (including the operator's `MAIAK_BLOCKED_CIDRS`), the connect-time `Control` check, no proxy. Hostnames are mapped to loopback through the existing `Options.Resolver`.
- Only `make test-e2e` (`go test -tags maiaktest -race -count=1 ./internal/e2e/...`) sets the tag; CI runs it in the `backend` job after `make test`. `make build` and the release images never set it, so the symbol does not exist there and any production reference would fail to compile.
- `TestOverrideNotSetOutsideTests` (normal build) proves it: the tagged file is excluded from a default `go/build` import of the package and included with the tag; no file of a normal build assigns the test-only fields or exports a name containing test, override, insecure or loopback; in the whole backend module only that file and `_test.go` files under `internal/e2e` carry the tag, and no other non-test file names the constructor. `TestLoopbackModeKeepsOtherRules` checks that the relaxation leaves the other rules active.
- `internal/e2e` runs api (httptest), one worker iteration at a time and one jobs delivery iteration at a time in one test binary against a database created by `dbbootstrap` and migrated as `maiak_owner`. A test resolver maps the target host and `hooks.slack.com` to loopback; a self-signed CA valid for both names is the client's root.

## Consequences
- The prober, the notifier and their error classification are exercised through the guard with real TLS, and a subtest proves a normal client ends the same loopback target as `blocked_address`.
- `golangci-lint` and `go vet ./...` without the tag do not see the e2e test; `go vet -tags maiaktest ./...` does. Closes L-030 and L-041.

## Alternatives considered
- An exported option in normal builds guarded by a convention: one careless assignment in `main.go` disables the guard.
- A plain client in the e2e test: does not test the guard, which is the point of L-041.
- Running the targets on 443 with root or a capability: not portable to CI and local runs.
