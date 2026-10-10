#!/usr/bin/env bash
# Render assertions for helm/cluster-manager: rules the templates encode that
# `helm lint` and the values schema cannot check. Runs in the chart workflow
# (.github/workflows/chart.yml) on every change and as `make helm-verify`.
# Needs helm.
set -euo pipefail
cd "$(dirname "$0")/.."
CHART=helm/cluster-manager

fail() { echo "verify-chart: FAIL: $*" >&2; exit 1; }

# The helm.sh/chart label is a valid label value (at most 63 characters,
# alphanumeric at both ends) for any chart version: the 63-character cut of
# "<name>-<version>" for a long version (a branch build's
# <version>-dev.<branch>.<date>.<time>.<sha>, or the <version>+<digest>
# helm-controller installs) can land on ".", on "_" (from "+") or on a run
# like "--.". Each version renders every object with the label it names. The
# version is set by packaging, since `helm template --version` does not apply
# to a chart directory.
pkg=$(mktemp -d)
trap 'rm -rf "$pkg"' EXIT
label_re='^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$'
while read -r v want; do
  helm package "$CHART" --version "$v" -d "$pkg" >/dev/null
  labels=$(helm template t "$pkg/$(basename "$CHART")-$v.tgz" | sed -n 's/^ *helm\.sh\/chart: *//p' | tr -d '"' | sort -u)
  [ -n "$labels" ] || fail "version $v renders no helm.sh/chart label"
  while IFS= read -r l; do
    [[ ${#l} -le 63 && $l =~ $label_re ]] || fail "version $v renders helm.sh/chart '$l', not a valid label value"
  done <<<"$labels"
  [ "$labels" = "$want" ] || fail "version $v renders helm.sh/chart '$labels', want '$want'"
done <<'VERSIONS'
0.1.0 cluster-manager-0.1.0
0.1.1-dev.renovate-helm-un.2026-09-22.14-54-24.h1a2b3c4 cluster-manager-0.1.1-dev.renovate-helm-un.2026-09-22.14-54-24
0.1.1-dev.renovate-helm-un.2026-09-22.14-54-24+h1a2b3c4 cluster-manager-0.1.1-dev.renovate-helm-un.2026-09-22.14-54-24
0.1.1-dev.renovate-helm-un.2026-09-22.14-54---.h1a2b3c4 cluster-manager-0.1.1-dev.renovate-helm-un.2026-09-22.14-54
VERSIONS

# OTLP trace export is opt-in: no OTEL_ variable but the metrics exporter's
# without observability.otel.endpoint, and with it the exporter, the
# parent-based sampler and the pod's k8s resource attributes.
if helm template t "$CHART" | grep -v -- 'OTEL_METRICS_EXPORTER\|OTEL_EXPORTER_PROMETHEUS_' | grep -q 'OTEL_'; then
  fail "the default render sets an OTEL_ variable other than the metrics exporter"
fi
otel=$(helm template t "$CHART" --set observability.otel.endpoint=http://otlp-gateway.kube-system.svc:4317 --set observability.otel.headers=X-Scope-OrgID=giantswarm)
for want in \
  'OTEL_EXPORTER_OTLP_ENDPOINT' 'value: "http://otlp-gateway.kube-system.svc:4317"' \
  'OTEL_EXPORTER_OTLP_PROTOCOL' 'value: "grpc"' \
  'OTEL_EXPORTER_OTLP_HEADERS' 'value: "X-Scope-OrgID=giantswarm"' \
  'OTEL_TRACES_SAMPLER' 'value: "parentbased_traceidratio"' \
  'OTEL_TRACES_SAMPLER_ARG' 'value: "0.1"' \
  'value: "k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE),k8s.node.name=$(NODE_NAME)"'; do
  grep -qF -- "$want" <<<"$otel" || fail "observability.otel.endpoint renders no '$want'"
done

# Metrics: the Prometheus exporter on the metrics port by default; off, the
# exporter is none, so an OTLP endpoint set for traces does not push metrics.
got=$(helm template t "$CHART")
echo "$got" | grep -A1 -- 'name: OTEL_METRICS_EXPORTER$' | grep -q -- 'value: prometheus' \
  || fail "default render: metrics on by default, want OTEL_METRICS_EXPORTER=prometheus"
echo "$got" | grep -q -- 'name: metrics$' || fail "default render: no metrics container port"
got=$(helm template t "$CHART" --set observability.metrics.enabled=false --set observability.otel.endpoint=http://otlp-gateway.kube-system.svc:4317)
echo "$got" | grep -A1 -- 'name: OTEL_METRICS_EXPORTER$' | grep -q -- 'value: none' \
  || fail "metrics off: want OTEL_METRICS_EXPORTER=none"
if echo "$got" | grep -q -- 'name: metrics$'; then fail "metrics port rendered with metrics off"; fi
got=$(helm template t "$CHART" --set serviceMonitor.enabled=true --set observability.metrics.enabled=false)
if echo "$got" | grep -q -- 'kind: ServiceMonitor'; then fail "ServiceMonitor rendered with metrics off"; fi
got=$(helm template t "$CHART" --show-only templates/servicemonitor.yaml --set serviceMonitor.enabled=true \
  --set-json 'serviceMonitor.labels={"observability.giantswarm.io/tenant":"giantswarm"}')
echo "$got" | grep -q -- '^    observability.giantswarm.io/tenant: giantswarm$' \
  || fail "ServiceMonitor lacks serviceMonitor.labels"
echo "$got" | grep -q -- '- port: metrics$' || fail "ServiceMonitor does not scrape the metrics port"

# Created clusters trust the installation's Dex: the audience is
# createdClusters.oidc.clientID, else muster's first required audience; with
# neither, no --cluster-oidc-client-id and created clusters trust none.
dex=(--set oauth.enabled=true --set oauth.dex.issuerURL=https://dex.example --set oauth.dex.clientID=platform --set oauth.baseURL=https://cluster-manager.example --set oauth.existingSecret=s)
got=$(helm template t "$CHART" "${dex[@]}" --set 'muster.mcpServer.auth.requiredAudiences={dex-k8s-authenticator}')
grep -qF -- '- --cluster-oidc-client-id=dex-k8s-authenticator' <<<"$got" || fail "created clusters: muster's required audience is not the OIDC client id"
got=$(helm template t "$CHART" "${dex[@]}" --set 'muster.mcpServer.auth.requiredAudiences={dex-k8s-authenticator}' --set createdClusters.oidc.clientID=kubernetes)
grep -qF -- '- --cluster-oidc-client-id=kubernetes' <<<"$got" || fail "created clusters: createdClusters.oidc.clientID does not win"
if helm template t "$CHART" "${dex[@]}" | grep -q -- '--cluster-oidc-'; then fail "created clusters: an OIDC client id rendered without an audience"; fi

# The tmp emptyDir at /tmp satisfies Kyverno's require-emptydir-requests-and-
# limits policy by default: the container requests and limits ephemeral-storage
# and the volume carries a sizeLimit; an empty tmpVolume.sizeLimit renders the
# bare emptyDir, the container fields alone still satisfying the rule.
got=$(helm template t "$CHART" --show-only templates/deployment.yaml)
for want in \
  '^              ephemeral-storage: 32Mi$' \
  '^              ephemeral-storage: 128Mi$' \
  '^            sizeLimit: 128Mi$'; do
  grep -q -- "$want" <<<"$got" || fail "default render: no '$want' for the tmp emptyDir"
done
got=$(helm template t "$CHART" --show-only templates/deployment.yaml --set tmpVolume.sizeLimit=)
grep -q -- '^          emptyDir: {}$' <<<"$got" || fail "tmpVolume.sizeLimit empty: the tmp volume is not a bare emptyDir"
if grep -q -- 'sizeLimit' <<<"$got"; then fail "tmpVolume.sizeLimit empty: a sizeLimit rendered"; fi
grep -q -- '^              ephemeral-storage: 128Mi$' <<<"$got" || fail "tmpVolume.sizeLimit empty: the container's ephemeral-storage limit is gone"

echo "verify-chart: ok"
