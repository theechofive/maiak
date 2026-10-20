# ADR-0002: One monorepo for backend and frontend

- Status: Accepted
- Date: 2026-10-20

## Context
Backend (Go) and frontend (React) are written by the same person. A future agent shares code with the backend. Comparable open-source monitoring projects (Gatus, Checkmate, Woodpecker) keep backend, frontend and agent in one repository.

## Decision
One repository `maiak`: `backend/`, `web/`, `api/openapi.yaml`, `deploy/helm/`, `docs/`. One Helm chart and one ArgoCD Application. The API contract lives next to the code; CI checks that generated code is up to date. CI jobs use path filters so a change in `web/` does not rebuild the backend.

## Consequences
- No release artifacts or version pinning between repos for the OpenAPI file.
- Shared access control for the whole repository.
- If a separate frontend team appears, `web/` can be extracted.

## Alternatives considered
- Two repositories: contract sync via release artifacts, a third repo or cross-repo CI writes, no benefit while one person owns both.
