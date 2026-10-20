#!/usr/bin/env python3
"""Asserts security and configuration invariants on a rendered Maiak chart (D-03, D-06).

Usage: check-rendered-chart.py rendered.yaml [more.yaml ...]
Exits 1 and prints every violated invariant. Needs PyYAML.
"""
import re
import sys

import yaml

ENV_NAMES_ENCRYPTION = {"MAIAK_ENCRYPTION_KEYS", "MAIAK_ENCRYPTION_ACTIVE_KEY_ID"}
ADMIN_ENV = {"MAIAK_ADMIN_EMAIL", "MAIAK_ADMIN_PASSWORD_HASH"}
# The worker is the only process that runs tenant-supplied targets; the processes below must
# not see anything the worker has not been given (ADR-0016, L-042).
ENCRYPTION_COMPONENTS = {"api", "jobs"}


def mem_mi(value):
    m = re.fullmatch(r"(\d+)(Mi|Gi)", str(value))
    if not m:
        raise ValueError(f"unsupported memory quantity {value!r}")
    return int(m.group(1)) * (1024 if m.group(2) == "Gi" else 1)


def pod_spec(doc):
    kind = doc["kind"]
    if kind in ("Deployment", "StatefulSet", "DaemonSet"):
        return doc["spec"]["template"]["spec"]
    if kind == "Job":
        return doc["spec"]["template"]["spec"]
    return None


def component(doc):
    return doc["metadata"].get("labels", {}).get("app.kubernetes.io/component", "")


def check(path):
    errors = []

    def fail(msg):
        errors.append(f"{path}: {msg}")

    with open(path) as f:
        docs = [d for d in yaml.safe_load_all(f) if d]

    kinds = [d["kind"] for d in docs]
    if "Ingress" in kinds:
        fail("Ingress object rendered (Gateway API only)")

    encryption_users, owner_users, token_pods = set(), set(), set()
    deployments = {}
    for d in docs:
        spec = pod_spec(d)
        if spec is None:
            continue
        name, comp = d["metadata"]["name"], component(d)
        if d["kind"] == "Deployment":
            deployments[comp] = d
        for flag in ("hostNetwork", "hostPID", "hostIPC"):
            if spec.get(flag):
                fail(f"{name}: {flag} is set")
        if spec.get("automountServiceAccountToken") is not False:
            token_pods.add(comp)
        if not spec.get("securityContext", {}).get("runAsNonRoot"):
            fail(f"{name}: pod securityContext.runAsNonRoot is not true")
        if spec.get("imagePullSecrets"):
            fail(f"{name}: imagePullSecrets set (L-002 is undecided; the chart must default to none)")
        for c in spec["containers"]:
            sc = c.get("securityContext", {})
            if sc.get("allowPrivilegeEscalation") is not False:
                fail(f"{name}/{c['name']}: allowPrivilegeEscalation is not false")
            if sc.get("readOnlyRootFilesystem") is not True:
                fail(f"{name}/{c['name']}: readOnlyRootFilesystem is not true")
            if sc.get("capabilities", {}).get("drop") != ["ALL"]:
                fail(f"{name}/{c['name']}: capabilities.drop is not [ALL]")
            if sc.get("seccompProfile", {}).get("type") != "RuntimeDefault":
                fail(f"{name}/{c['name']}: seccompProfile is not RuntimeDefault")
            if sc.get("privileged"):
                fail(f"{name}/{c['name']}: privileged")
            env = {e["name"]: e for e in c.get("env", [])}
            if ENV_NAMES_ENCRYPTION & set(env):
                encryption_users.add(comp)
            for e in env.values():
                ref = e.get("valueFrom", {}).get("secretKeyRef", {})
                if ref.get("key") == "owner_url":
                    owner_users.add((d["kind"], comp))
                n = e["name"]
                if re.search(r"PASSWORD|KEYS|DATABASE_URL", n) and "valueFrom" not in e:
                    fail(f"{name}/{c['name']}: {n} is a literal, not a secretKeyRef")
            res = c.get("resources", {})
            if d["kind"] == "Deployment":
                for kind_ in ("requests", "limits"):
                    for r in ("cpu", "memory"):
                        if kind_ == "limits" and r == "cpu":
                            continue
                        if r not in res.get(kind_, {}):
                            fail(f"{name}/{c['name']}: resources.{kind_}.{r} missing")
                try:
                    ratio = mem_mi(res["limits"]["memory"]) / mem_mi(res["requests"]["memory"])
                    if not 3 <= ratio <= 4:
                        fail(f"{name}/{c['name']}: memory limit is {ratio:g}x the request (want 3-4x)")
                except (KeyError, ValueError) as ex:
                    fail(f"{name}/{c['name']}: cannot compare memory ({ex})")
            if comp in ("api", "worker", "jobs"):
                paths = {p["httpGet"]["path"] for p in (c.get("livenessProbe", {}), c.get("readinessProbe", {})) if "httpGet" in p}
                if paths != {"/healthz", "/readyz"}:
                    fail(f"{name}: probes should use /healthz and /readyz, got {sorted(paths)}")
            if comp == "web":
                for probe in ("livenessProbe", "readinessProbe"):
                    hg = c.get(probe, {}).get("httpGet", {})
                    if hg.get("path") != "/healthz" or hg.get("port") != "http":
                        fail(f"{name}: {probe} must GET /healthz on port http")
                mounts = {m["mountPath"] for m in c.get("volumeMounts", [])}
                if mounts != {"/tmp"}:
                    fail(f"{name}: web should mount only /tmp, got {sorted(mounts)}")

    # Env contract per workload (config.go).
    def env_of(comp):
        d = deployments.get(comp)
        return {e["name"]: e for e in d["spec"]["template"]["spec"]["containers"][0].get("env", [])} if d else {}

    if encryption_users != ENCRYPTION_COMPONENTS:
        fail(f"MAIAK_ENCRYPTION_* present in {sorted(encryption_users)}, want exactly {sorted(ENCRYPTION_COMPONENTS)}")
    worker = env_of("worker")
    if not worker:
        fail("no worker Deployment")
    if ADMIN_ENV & set(worker) or any(n.startswith("MAIAK_ENCRYPTION_") for n in worker):
        fail("worker has admin credentials or encryption keys")
    for n in ("MAIAK_WORKER_CLAIM_BATCH", "MAIAK_WORKER_CONCURRENCY", "MAIAK_WORKER_TENANT_CONCURRENCY", "MAIAK_WORKER_HOST_CONCURRENCY", "MAIAK_WORKER_LOCATION"):
        if n not in worker:
            fail(f"worker lacks {n}")
    api, jobs = env_of("api"), env_of("jobs")
    for n in ("MAIAK_ADMIN_EMAIL", "MAIAK_ADMIN_PASSWORD_HASH", "MAIAK_SESSION_TTL", "MAIAK_COOKIE_SECURE", "MAIAK_TRUSTED_PROXY_HOPS", "MAIAK_PUBLIC_URL", "MAIAK_HTTP_ADDR"):
        if n not in api:
            fail(f"api lacks {n}")
    if ADMIN_ENV & set(jobs):
        fail("jobs has admin credentials")
    for n in ("MAIAK_PUBLIC_URL", "MAIAK_ENCRYPTION_KEYS"):
        if n not in jobs:
            fail(f"jobs lacks {n}")
    if "MAIAK_PUBLIC_URL" in worker:
        fail("worker has MAIAK_PUBLIC_URL")
    for comp, env in (("api", api), ("jobs", jobs)):
        url = env.get("MAIAK_PUBLIC_URL", {}).get("value", "")
        if not url.startswith("https://") or url.endswith("/"):
            fail(f"{comp}: MAIAK_PUBLIC_URL {url!r} must be https without trailing slash")
    for comp, d in deployments.items():
        grace = d["spec"]["template"]["spec"].get("terminationGracePeriodSeconds")
        if comp in ("worker", "jobs") and grace != 45:
            fail(f"{comp}: terminationGracePeriodSeconds is {grace}, want 45")

    # Owner URL only for the migrate Job.
    if owner_users != {("Job", "migrate")}:
        fail(f"owner_url used by {sorted(owner_users)}, want only the migrate Job")
    for d in docs:
        spec = pod_spec(d)
        if spec and d["kind"] == "Job" and component(d) == "migrate":
            names = [e["name"] for e in spec["containers"][0]["env"]]
            if "MAIAK_MIGRATE_DATABASE_URL" not in names or "MAIAK_DATABASE_URL" in names:
                fail("migrate Job env must contain MAIAK_MIGRATE_DATABASE_URL only")
        elif spec and "MAIAK_MIGRATE_DATABASE_URL" in str(spec["containers"]):
            fail(f"{d['metadata']['name']} has MAIAK_MIGRATE_DATABASE_URL")

    # Token automount: only the db-bootstrap ServiceAccount and Job.
    for d in docs:
        if d["kind"] == "ServiceAccount":
            mounts = d.get("automountServiceAccountToken")
            if mounts is True and component(d) != "db-bootstrap":
                fail(f"ServiceAccount {d['metadata']['name']} automounts a token")
            if mounts is not True and mounts is not False:
                fail(f"ServiceAccount {d['metadata']['name']} does not set automountServiceAccountToken")
            if component(d) == "db-bootstrap" and mounts is not True:
                fail("db-bootstrap ServiceAccount must automount its token")
    if token_pods - {"db-bootstrap"}:
        fail(f"pods without automountServiceAccountToken=false: {sorted(token_pods - {'db-bootstrap'})}")

    # Web and routing.
    web = "web" in deployments
    routes = [d for d in docs if d["kind"] == "HTTPRoute"]
    for r in routes:
        rules = r["spec"]["rules"]
        by_path = {m["path"]["value"]: (rule, rule["backendRefs"][0]["name"]) for rule in rules for m in rule["matches"]}
        if "/api" not in by_path or not by_path["/api"][1].endswith("-api"):
            fail("HTTPRoute: /api must go to the api")
        elif by_path["/api"][0].get("timeouts", {}).get("request") != "60s":
            fail("HTTPRoute: /api request timeout must be 60s")
        if web and not ("/" in by_path and by_path["/"][1].endswith("-web")):
            fail("HTTPRoute: / must go to web")
        if not web and "/" in by_path:
            fail("HTTPRoute: / routed although web is disabled")
    return errors


def main():
    errors = []
    for p in sys.argv[1:]:
        errors += check(p)
    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(f"check-rendered-chart: ok ({len(sys.argv) - 1} render(s))")
    return 0


if __name__ == "__main__":
    sys.exit(main())
