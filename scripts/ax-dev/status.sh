#!/usr/bin/env bash
# Copyright (c) 2026 Yu YI
# SPDX-License-Identifier: MIT
# scripts/ax-dev/status.sh — verify ate-system, ax-system, and ax-workers readiness.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

QUIET="false"
if [[ "${1:-}" == "--quiet" || "${1:-}" == "-q" ]]; then
  QUIET="true"
fi

k() { kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"; }

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  printf 'ERROR: kind cluster %q is not running (run scripts/ax-dev/up.sh).\n' "${KIND_CLUSTER_NAME}" >&2
  exit 1
fi

if [[ "${QUIET}" == "false" ]]; then
  sub_sha="$(k get namespace ate-system -o jsonpath='{.metadata.annotations.substrate-commit}' 2>/dev/null || true)"
  ax_spec="$(k get namespace "${AX_SYSTEM_NAMESPACE}" -o jsonpath='{.metadata.annotations.glue\.ax-dev/spec}' 2>/dev/null || true)"
  printf '=== Cluster: %s (context %s, substrate=%s, ax=%s) ===\n' \
    "${KIND_CLUSTER_NAME}" "${KUBE_CONTEXT}" "${sub_sha:0:12}" "${ax_spec}"
  k get nodes -o wide
fi

for ns in ate-system "${AX_SYSTEM_NAMESPACE}"; do
  if [[ "${QUIET}" == "false" ]]; then
    printf '\n=== Namespace: %s ===\n' "${ns}"
    k -n "${ns}" get pods
  fi
  k -n "${ns}" wait --for=condition=Ready pod --field-selector=status.phase!=Succeeded --timeout=60s >/dev/null
done

if [[ "${QUIET}" == "false" ]]; then
  printf '\n=== WorkerPool: %s/ax-workers ===\n' "${AX_SYSTEM_NAMESPACE}"
  k -n "${AX_SYSTEM_NAMESPACE}" get workerpool ax-workers
fi
k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-workers --timeout=30s >/dev/null

if [[ "${QUIET}" == "false" && -f "${STATE}/images.env" ]]; then
  printf '\n=== Pinned Images (%s/images.env) ===\n' "${STATE}"
  cat "${STATE}/images.env"
fi
printf '\nSTATUS OK: all ate-system and %s pods Ready; worker pool ax-workers Ready.\n' "${AX_SYSTEM_NAMESPACE}"
