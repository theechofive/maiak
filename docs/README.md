# Documentation index

- [Project brief](PROJECT.md): goal, MVP scope, decisions at a glance, delivery plan, ownership. Start here.
- [Architecture overview](architecture/overview.md): components, processes, flows, ownership.
- [Data model](architecture/data-model.md): tables, tenancy, partitioning, retention.
- [ADRs](adr/): one architectural decision per file, with the reasoning.
- [MVP task lists](tasks/README.md): backend, frontend, deploy.
- [Ledger](LEDGER.md): deferred work, problems and risks, with revisit triggers.
- [Runbooks](runbooks/): operational procedures.

Rules: a new architectural decision gets an ADR (copy `adr/0000-template.md`). Anything deferred or found broken goes into the ledger in the same PR.
