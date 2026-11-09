# ADR-0105: Open core, editions and entitlements

Status: accepted (2026-11-09), with the items marked *proposed* below to be confirmed before the first public release; the licence of the core is **to be decided before the first public release**.

## Context
Maiak is built as a multi-tenant SaaS starter in one private repository that mixes product code with the operations of one installation (`klr-platform`). The owner wants the product to be usable and inspectable by others, and to sell paid features and a hosted service without maintaining two products. Comparable products solved the same problem with open core: GitLab (CE/EE, `ee/` directories, one codebase), Metabase, PostHog, Infisical and n8n (one binary, a licence key unlocks paid features).

## Decision
Status per item: **accepted** items are decided; **proposed** items are the current intention, to be confirmed by the owner before the first public release.

**Repositories.**
- *Accepted:* one **public** repository with the core and the paid modules. The core is published under AGPL-3.0 or FSL; the choice is open and must be made before the first public release (ledger L-313). Paid modules live in `ee/` directories (for example `backend/ee/...`, `web/src/ee/...`) under a separate source-available Enterprise licence.
- *Accepted:* one **private** repository (`maiak-cloud`) holding the SaaS operations: deployment values and ArgoCD wiring, internal runbooks, the billing service (it changes plans through the admin API, never the database), the licence issuing tool and the private signing key, and the management of the hosted probe fleet. It also holds any paid module that is **not yet approved for publication**: where a future paid module lives is decided per module, and the default is private until proven otherwise, because public history cannot be closed again. When the first such module appears, the enterprise build gets an overlay step that adds it from the private repository; that step gets its own ADR then.
- *Proposed:* **Apache-2.0** for the parts others embed: agents and probes, the Helm chart and the generated API client.
- *Proposed:* a trademark policy file in the public repository (the name and logo are not covered by the code licences).
- *Proposed:* if the core is AGPL, contributions to the core need a CLA (so the owner can also license the core commercially). External contributions to `ee/` are not accepted.

**Entitlements** (accepted). Paid features are named strings (`sso`, `audit-export`, ...: lowercase slugs). Code asks `entitlements.Has(tenant, feature)`; it never asks which plan or licence the tenant has.
- SaaS: the features come from the tenant's plan (`plans.features`, O-08).
- Self-hosted: from an offline-verified signed licence key (Ed25519, public key compiled into the enterprise image). On expiry the service keeps running and the `ee` features turn off (soft disable); data is never deleted or locked.
- The api reports the organization's features in the session (`features` on `GET /auth/me` and the other session answers) so the web app hides what is not available; the api enforces every paid operation itself. The web app keeps the features of the session until it asks `GET /auth/me` again, so a plan change shows up in the UI late; the api applies it at once.

**Images** (accepted). Two images from one codebase: **community**, built without `ee` code (Go build tag, web build without the `ee` entry), and **enterprise**, built with it. In self-hosted mode the enterprise image without a licence key behaves like community; in the SaaS the features come from the plan, not from a key.

**Deployment modes** (accepted). Same code; the difference is instance configuration and packaging. Tenancy and RLS are identical in both modes.
- *Self-hosted:* one company per installation, registration closed, the `community` plan without quotas (`plans.Unlimited` limits), the first owner created at first run instead of the operator's `default` organization flow.
- *SaaS:* open sign-up, plans with quotas and billing, hosted probes.

**Requirements for the future `ee/LICENSE`** (proposed): no circumvention of the licence key check; termination on breach; an audit right for contract customers; canary strings in `ee` code to recognise copies; and an explicit statement that access to the files is not permission to use them.

## Consequences
- Code that serves one installation (hostnames, cluster names, AWS profiles, secret paths) must leave the public repository; the inventory is `docs/tasks/repo-split.md`. The project name changes before the split (rename task); the same file lists every place the name appears.
- O-08 adds the seam only: `plans.features`, the `community` plan, `internal/entitlements` (`Features`, `Has`), the session field. There is no licence-key code and no `ee/` directory yet (L-312).
- Every new paid capability is designed as a feature name first, with the check in the api and the hiding in the web app.
- Choosing AGPL implies a CLA process before the first external contribution; choosing FSL implies a delayed open-source date per release.

## Alternatives considered
- **Fully open source** (no paid code): no revenue beyond hosting, and hosting alone is easy to copy.
- **Fully closed SaaS:** no community edition, no self-hosted market, nothing others can verify.
- **Two codebases** (a fork for enterprise): every fix twice, and they drift.
- **A separate private `maiak-ee` repository for all paid code:** keeps paid code private, but customers cannot read what they pay for and every build needs two repositories. Not the default: paid modules go to the public `ee/`; only a module not yet approved for publication waits in `maiak-cloud` (see Decision).
- **Licence check online** (call home): breaks air-gapped installations and fails closed when our service is down.
