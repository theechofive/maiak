# ADR-0001: Hosted multi-tenant SaaS with agents for internal services

- Status: Accepted
- Date: 2026-10-20

## Context
The goal is a monitoring product for many clients, covering public sites and services inside the clients' Kubernetes clusters. A central hosted service cannot reach a client's internal services.

## Decision
Build a centrally hosted multi-tenant SaaS. External HTTPS checks run from our probes. Internal services are covered later by a lightweight `agent` the client installs (Helm chart) in their cluster, which pulls jobs and pushes results over HTTPS. The MVP has only external HTTPS checks, one admin user, Slack alerts and a private dashboard. Status page, agent, TCP/DNS/k8s checks, multiple users, OAuth and billing are deferred (ledger L-004..L-008, L-014).

## Consequences
- Multi-tenancy is designed in from day one even though the MVP has one tenant (ADR-0004).
- The probe protocol must also serve agents (ADR-0006).
- Public signup needs abuse controls first (L-011).

## Alternatives considered
- Self-hosted per-client install: no SaaS economics, rejected.
- Internal-only tool: contradicts the stated goal of a SaaS starter.
