# ADR-0008: REST with OpenAPI, spec-first, same-origin routing

- Status: Accepted
- Date: 2026-10-20

## Context
The web app and the backend live in one repository but are developed separately in time. A public API for customers is likely later.

## Decision
REST with JSON under `/api/v1`. `api/openapi.yaml` is the source of truth: Go server code is generated with oapi-codegen, the TypeScript client is generated from the same file, CI lints the spec and runs `oasdiff` for breaking changes. The browser uses one origin: the gateway routes `/api` to the api and `/` to the web, so there is no CORS and the session can be an HttpOnly, Secure, SameSite=Lax cookie with a CSRF header on unsafe methods. Dashboard freshness uses polling every 15-30 s; SSE may be added later as `GET /api/v1/events` without changing existing endpoints. Errors are `application/problem+json` (RFC 9457).

## Consequences
- Cookie auth needs CSRF protection (header token checked in constant time).
- API keys for customers will be added alongside cookies later.

## Alternatives considered
- GraphQL or gRPC/Connect: more tooling for no current benefit.
- Separate subdomains for app and api: needs CORS and more cookie care.
- WebSocket for live updates: bidirectional channel is unnecessary for a server-to-browser feed.
