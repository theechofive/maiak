# ADR-0011: SSRF defence implemented in the worker code

- Status: Accepted (amended by ADR-0016: port 443 only in the MVP, Slack delivery uses the same guard; remaining details open: L-001)
- Date: 2026-10-20

## Context
In a hosted SaaS a tenant chooses the URL our worker fetches from inside our network. Targets such as cluster services, internal hosts or the cloud metadata endpoint must not be reachable. The cluster's CNI does not enforce NetworkPolicy (R-002). Nodes use IMDSv2 with hop limit 1, which protects pods that do not use hostNetwork.

## Decision
- Validate the destination IP at connect time (a custom dialer `Control` hook), not only when parsing the URL, to defeat DNS rebinding.
- Block loopback, private (`10/8`, `172.16/12`, `192.168/16`), link-local (`169.254/16`), CGNAT (`100.64/10`), `0/8`, multicast, `::1`, `fc00::/7`, `fe80::/10` and IPv4-mapped IPv6. Extra CIDRs configurable by environment variable.
- At most 3 redirects, each re-validated, HTTPS only.
- Ignore proxy environment variables (`Proxy: nil`).
- Do not read the response body by default (cap 64 KB only when a keyword check is configured), never return it to users, return generic errors.
- Worker runs with its own ServiceAccount without permissions, `automountServiceAccountToken: false`, never `hostNetwork`, no AWS role.
- Checks of private networks are only possible through the future agent running inside the client's network; there is no private-IP exception on the platform.

## Consequences
- The guard code is security-critical and is covered by table-driven tests including rebinding.
- Port policy (443/8443) and error wording are still open (L-001).

## Alternatives considered
- Relying on NetworkPolicy: not enforced in the cluster.
- Validating only the URL host: bypassable by DNS rebinding.
