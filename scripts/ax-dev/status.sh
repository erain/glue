#!/usr/bin/env bash
# scripts/ax-dev/status.sh — verify ate-system, ax-system, and ax-workers readiness.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

k() { kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"; }

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  printf 'ERROR: kind cluster %q is not running (run scripts/ax-dev/up.sh).\n' "${KIND_CLUSTER_NAME}" >&2
  exit 1
fi

printf '=== Cluster: %s (context %s) ===\n' "${KIND_CLUSTER_NAME}" "${KUBE_CONTEXT}"
k get nodes -o wide

for ns in ate-system "${AX_SYSTEM_NAMESPACE}"; do
  printf '\n=== Namespace: %s ===\n' "${ns}"
  k -n "${ns}" get pods
  k -n "${ns}" wait --for=condition=Ready pod --field-selector=status.phase!=Succeeded --timeout=60s >/dev/null
done

printf '\n=== WorkerPool: %s/ax-workers ===\n' "${AX_SYSTEM_NAMESPACE}"
k -n "${AX_SYSTEM_NAMESPACE}" get workerpool ax-workers
k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-workers --timeout=30s

if [[ -f "${STATE}/images.env" ]]; then
  printf '\n=== Pinned Images (%s/images.env) ===\n' "${STATE}"
  cat "${STATE}/images.env"
fi
printf '\nSTATUS OK: all ate-system and %s pods Ready; worker pool ax-workers Ready.\n' "${AX_SYSTEM_NAMESPACE}"
