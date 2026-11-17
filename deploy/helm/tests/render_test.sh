#!/usr/bin/env bash
# Render assertions for the ATC Helm chart. Run from the repository root:
#   deploy/helm/tests/render_test.sh
set -euo pipefail

chart=deploy/helm/atc
fail=0

check() { # check <description> <rendered> <grep -E pattern> [absent]
  local desc=$1 out=$2 pattern=$3 mode=${4:-present}
  if grep -Eq -- "$pattern" <<<"$out"; then found=1; else found=0; fi
  if [[ $mode == present && $found == 1 ]] || [[ $mode == absent && $found == 0 ]]; then
    echo "ok   - $desc"
  else
    echo "FAIL - $desc (expected pattern $mode: $pattern)"; fail=1
  fi
}

defaults=$(helm template t "$chart")
check "image defaults to the published ghcr.io image" "$defaults" 'image: "ghcr\.io/atcprojectio/atc:[0-9]'
check "image tag has no v prefix" "$defaults" 'image: "ghcr\.io/atcprojectio/atc:v' absent
check "Consul address defaults to the node-local agent" "$defaults" 'value: "http://\$\(HOST_IP\):8500"'
check "HOST_IP comes from the downward API" "$defaults" 'fieldPath: status\.hostIP'
check "no token Secret by default" "$defaults" 'ATC_CONSUL_TOKEN' absent
check "runs as non-root" "$defaults" 'runAsNonRoot: true'
check "read-only root filesystem" "$defaults" 'readOnlyRootFilesystem: true'

custom=$(helm template t "$chart" \
  --set config.consulAddr=http://consul-server.consul.svc:8500 \
  --set consulToken.existingSecret=atc-consul \
  --set consulToken.key=acl-token \
  --set 'extraEnv[0].name=OTEL_EXPORTER_OTLP_ENDPOINT' \
  --set 'extraEnv[0].value=http://otel:4318')
check "explicit Consul address is used" "$custom" 'value: "http://consul-server\.consul\.svc:8500"'
check "token is read from the Secret" "$custom" 'name: atc-consul'
check "token Secret key is configurable" "$custom" 'key: acl-token'
check "extraEnv is rendered" "$custom" 'name: OTEL_EXPORTER_OTLP_ENDPOINT'

# The published chart sets appVersion from the git tag; it must match the image tag format.
packaged=$(helm template t "$chart" --set image.tag="" | grep 'image:' || true)
check "appVersion drives the image tag" "$packaged" 'atc:[0-9]+\.[0-9]+\.[0-9]+'

# HA: replicas are spread over nodes and protected during drains.
check "PodDisruptionBudget rendered for 2 replicas" "$defaults" 'kind: PodDisruptionBudget'
check "PDB allows at most one replica down" "$defaults" 'maxUnavailable: 1'
check "soft anti-affinity by default" "$defaults" 'preferredDuringSchedulingIgnoredDuringExecution'
check "anti-affinity spreads over nodes" "$defaults" 'topologyKey: kubernetes.io/hostname'
hard=$(helm template t "$chart" --set podAntiAffinity=hard)
check "hard anti-affinity on request" "$hard" 'requiredDuringSchedulingIgnoredDuringExecution'
single=$(helm template t "$chart" --set replicaCount=1)
check "no PDB for a single replica (it would block drains)" "$single" 'kind: PodDisruptionBudget' absent
check "no anti-affinity for a single replica" "$single" 'podAntiAffinity' absent
explicit=$(helm template t "$chart" --set 'affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=pool' --set 'affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists')
check "explicit affinity replaces the default" "$explicit" 'nodeAffinity'
check "explicit affinity has no default anti-affinity" "$explicit" 'podAntiAffinity' absent

# Simulate the release workflow: the chart is packaged from a git tag like "v0.5.0".
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
GITHUB_REF_NAME=v0.5.0
helm package "$chart" --version "${GITHUB_REF_NAME#v}" --app-version "${GITHUB_REF_NAME#v}" -d "$tmp" >/dev/null
released=$(helm template t "$tmp"/atc-0.5.0.tgz)
check "released chart pulls the image tag goreleaser publishes (0.5.0, not v0.5.0)" "$released" 'image: "ghcr\.io/atcprojectio/atc:0\.5\.0"'
grep -q -- '--app-version "${GITHUB_REF_NAME#v}"' .github/workflows/publish.yml \
  && echo "ok   - publish workflow strips the v prefix" \
  || { echo "FAIL - publish workflow must use --app-version \"\${GITHUB_REF_NAME#v}\""; fail=1; }

exit $fail
