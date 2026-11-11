#!/usr/bin/env bash
# scripts/ax-dev/down.sh — tear down the Glue AX kind cluster, registry, and tunnels.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

if command -v ax >/dev/null 2>&1; then
  ax tunnel stop "${KUBE_CONTEXT}" >/dev/null 2>&1 || true
fi
pkill -f "kubectl.*${KUBE_CONTEXT}.*port-forward" >/dev/null 2>&1 || true
pkill -f "kubectl-ate.*${KUBE_CONTEXT}" >/dev/null 2>&1 || true

if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  printf 'Deleting kind cluster %q...\n' "${KIND_CLUSTER_NAME}"
  kind delete cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}"
fi
rm -f "${STATE}/kubeconfig" "${STATE}/images.env"

if docker inspect "${REGISTRY_NAME}" >/dev/null 2>&1; then
  printf 'Removing registry container %q...\n' "${REGISTRY_NAME}"
  docker rm -f "${REGISTRY_NAME}" >/dev/null
fi
printf 'Teardown complete.\n'
