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

# OTLP export is opt-in: no OTEL_ variable without observability.otel.endpoint,
# and with it the exporter, the parent-based sampler and the pod's k8s
# resource attributes.
if helm template t "$CHART" | grep -q 'OTEL_'; then
  fail "the default render sets an OTEL_ variable"
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

# Two muster registrations with github.enabled: the main MCPServer forwards the
# IdP token (forwardToken) on the main path whatever github.enabled says, so
# nothing but commit mode waits for the GitHub App's consent; the
# <name>-commit MCPServer on the commit path pins the App (authorizationServer,
# forwardIdentity) and the server is told its name. Without github.enabled
# only the main one renders.
oauth=(--set muster.mcpServer.enabled=true --set oauth.enabled=true --set oauth.baseURL=https://cluster-manager.example.com
  --set oauth.dex.issuerURL=https://dex.example.com --set oauth.dex.clientID=platform --set oauth.existingSecret=oauth)
# mcpserver prints the MCPServer named $2 of the render $1.
mcpserver() { awk -v want="$2" '/^---/{if(doc ~ "\nkind: MCPServer\n" && doc ~ "\n  name: " want "\n")print doc; doc=""; next}{doc=doc"\n"$0}END{if(doc ~ "\nkind: MCPServer\n" && doc ~ "\n  name: " want "\n")print doc}' <<<"$1"; }
plain=$(helm template t "$CHART" "${oauth[@]}")
[ "$(grep -c '^kind: MCPServer$' <<<"$plain")" = 1 ] || fail "without github.enabled the chart renders other than one MCPServer"
pinned=$(helm template t "$CHART" "${oauth[@]}" --set github.enabled=true)
[ "$(grep -c '^kind: MCPServer$' <<<"$pinned")" = 2 ] || fail "github.enabled renders other than two MCPServers"
for render in "$plain" "$pinned"; do
  main=$(mcpserver "$render" cluster-manager)
  for want in 'url: http://t-cluster-manager.default.svc.cluster.local:8080/mcp' 'forwardToken: true'; do
    grep -qF -- "$want" <<<"$main" || fail "the main MCPServer renders no '$want'"
  done
  for unwanted in authorizationServer forwardIdentity; do
    if grep -qF -- "$unwanted" <<<"$main"; then fail "the main MCPServer renders '$unwanted'"; fi
  done
done
commit=$(mcpserver "$pinned" cluster-manager-commit)
for want in 'url: http://t-cluster-manager.default.svc.cluster.local:8080/commit/mcp' 'authorizationServer:' \
  'issuer: "https://github.com/apps/giantswarm-cluster-manager"' 'forwardIdentity: true'; do
  grep -qF -- "$want" <<<"$commit" || fail "the commit MCPServer renders no '$want'"
done
if grep -qF forwardToken <<<"$commit"; then fail "the commit MCPServer renders forwardToken"; fi
for want in '--commit-path=/commit/mcp' '--commit-registration=cluster-manager-commit'; do
  grep -qF -- "$want" <<<"$pinned" || fail "github.enabled passes no '$want'"
done
if grep -qF -- '--commit-' <<<"$plain"; then fail "without github.enabled the server is given a commit flag"; fi

echo "verify-chart: ok"
