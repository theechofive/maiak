#!/usr/bin/env bash
# Static validation of the Helm chart (D-06): helm lint and helm template for every value
# combination we ship, invariants on the rendered YAML, and kubeconform when it is installed.
# Never talks to a cluster. Run from anywhere: bash scripts/validate-chart.sh
#
# Optional environment:
#   CRD_SCHEMA_DIR  directory laid out like datreeio/CRDs-catalog ({group}/{kind}_{version}.json) with
#                   the Gateway API and External Secrets schemas; without it those kinds are skipped.
#   KUBERNETES_VERSION  schema version for kubeconform (default 1.31.0).
set -euo pipefail

cd "$(dirname "$0")/.."
chart=deploy/helm/maiak
platform=$chart/values-platform.yaml
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

# name|helm args
combos=(
  "defaults|"
  "defaults-web|--set web.enabled=true"
  "platform|-f $platform"
  "platform-no-bootstrap|-f $platform --set dbBootstrap.enabled=false"
  "platform-no-web|-f $platform --set web.enabled=false"
  "platform-login-set|-f $platform --set secrets.admin.login=owner+ops@example.com"
  "platform-registration-off|-f $platform --set config.registrationEnabled=false"
  "platform-external-secrets|-f $platform --set secrets.admin.source=externalSecret --set secrets.crypto.source=externalSecret"
  "platform-admin-external|-f $platform --set secrets.admin.source=externalSecret"
  "platform-crypto-external|-f $platform --set secrets.crypto.source=externalSecret"
  "platform-all-optional|-f $platform --set serviceMonitor.enabled=true --set networkPolicy.enabled=true --set api.pdb.enabled=true --set topologySpread.enabled=true --set config.blockedCIDRs[0]=100.64.0.0/10 --set config.blockedCIDRs[1]=198.18.0.0/15 --set worker.location=home-test"
)

renders=()
for combo in "${combos[@]}"; do
  name=${combo%%|*}
  args=${combo#*|}
  echo "== helm lint: $name"
  # shellcheck disable=SC2086  # args are intentionally word-split
  helm lint "$chart" $args >"$out/$name.lint" 2>&1 || { cat "$out/$name.lint"; exit 1; }
  tail -n 1 "$out/$name.lint"
  echo "== helm template: $name"
  # shellcheck disable=SC2086
  helm template maiak "$chart" -n maiak $args >"$out/$name.yaml"
  renders+=("$out/$name.yaml")
done

echo "== registration kill switch (MAIAK_REGISTRATION_ENABLED, api only)"
kill_switch() { grep -A1 'name: MAIAK_REGISTRATION_ENABLED' "$out/$1.yaml" | sed -n 's/.*value: //p'; }
[ "$(kill_switch platform)" = '"true"' ] || { echo "FAIL: registration not enabled by default" >&2; exit 1; }
[ "$(kill_switch platform-registration-off)" = '"false"' ] || { echo "FAIL: config.registrationEnabled=false not rendered" >&2; exit 1; }
echo "default \"true\", config.registrationEnabled=false renders \"false\""

echo "== negative checks (rendering must fail)"
expect_fail() {
  local label=$1
  shift
  if helm template maiak "$chart" -n maiak "$@" >/dev/null 2>&1; then
    echo "FAIL: $label rendered but must be rejected" >&2
    exit 1
  fi
  echo "rejected as expected: $label"
}
expect_fail "http public URL" --set config.publicUrl=http://maiak.example.com
expect_fail "public URL with trailing slash" --set config.publicUrl=https://maiak.example.com/
expect_fail "PreSync migration with bootstrap" -f "$platform" --set dbBootstrap.enabled=true --set migration.hookPhase=PreSync
expect_fail "bootstrap without host" --set dbBootstrap.enabled=true
expect_fail "memory limit missing" --set worker.resources.limits=null
expect_fail "unknown secrets source" --set secrets.admin.source=manual
expect_fail "upper-case admin login" --set secrets.admin.login=Admin
expect_fail "admin login with a space" --set "secrets.admin.login=a b"
expect_fail "admin login shorter than 3" --set secrets.admin.login=ab
expect_fail "registrationEnabled not a boolean" --set-string config.registrationEnabled=yes
expect_fail "empty admin login in generate mode" --set secrets.admin.login=
expect_fail "admin login removed in generate mode" -f "$platform" --set secrets.admin.login=null
expect_fail "invalid crypto key id" --set "secrets.crypto.keyId=k 1"
expect_fail "externalSecret without remote key" --set secrets.crypto.source=externalSecret --set secrets.crypto.remoteKey=

echo "== invariants on rendered manifests"
python3 scripts/check-rendered-chart.py "${renders[@]}"

echo "== kubeconform"
if ! command -v kubeconform >/dev/null 2>&1; then
  echo "kubeconform not installed: skipped (CI installs a pinned release)"
  exit 0
fi
kc=(kubeconform -strict -summary -kubernetes-version "${KUBERNETES_VERSION:-1.31.0}")
if [ -n "${CRD_SCHEMA_DIR:-}" ]; then
  kc+=(-schema-location default -schema-location "$CRD_SCHEMA_DIR/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json")
else
  echo "CRD_SCHEMA_DIR not set: skipping Gateway API (HTTPRoute), External Secrets (ExternalSecret) and Prometheus Operator (ServiceMonitor) kinds"
  kc+=(-skip "HTTPRoute,ExternalSecret,ServiceMonitor")
fi
"${kc[@]}" "${renders[@]}"
