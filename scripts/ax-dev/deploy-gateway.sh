#!/usr/bin/env bash
# scripts/ax-dev/deploy-gateway.sh — build, push, and deploy glue-gateway to glue-system on kind.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

log() { printf '==> %s\n' "$*" >&2; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"; }

[[ -f "${STATE}/images.env" ]] || fail "missing ${STATE}/images.env; run scripts/ax-dev/up.sh && scripts/ax-dev/build-task-image.sh first"
# shellcheck disable=SC1090,SC1091
source "${STATE}/images.env"
[[ -n "${GLUE_TASK_IMAGE:-}" ]] || fail "GLUE_TASK_IMAGE not set in ${STATE}/images.env; run scripts/ax-dev/build-task-image.sh first"
[[ -f "${STATE}/gateway.env" ]] && { # shellcheck disable=SC1090,SC1091
  source "${STATE}/gateway.env"; }

GITHUB_TOKEN="${GLUE_GITHUB_TOKEN:-}"
if [[ -z "${GITHUB_TOKEN}" ]] && command -v gh >/dev/null 2>&1; then
  GITHUB_TOKEN="$(gh auth token 2>/dev/null || true)"
fi
[[ -n "${GITHUB_TOKEN}" ]] || fail "GLUE_GITHUB_TOKEN is unset and 'gh auth token' returned empty"
if [[ -z "${GEMINI_API_KEY:-}" && -z "${OPENROUTER_API_KEY:-}" && -f "${HOME}/.env" ]]; then
  # shellcheck disable=SC1091
  set -a; source "${HOME}/.env"; set +a
fi
[[ -n "${GEMINI_API_KEY:-}" || -n "${OPENROUTER_API_KEY:-}" ]] || fail "GEMINI_API_KEY (or OPENROUTER_API_KEY) must be set in the environment"
GATEWAY_TOKEN="${GLUE_GATEWAY_TOKEN:-$(openssl rand -hex 24)}"

export GOARCH
GOARCH="$(go env GOARCH)"
mkdir -p "${STATE}/docker-config"
if [[ -z "${DOCKER_CONFIG:-}" ]]; then
  export DOCKER_CONFIG="${STATE}/docker-config"
  [[ -f "${DOCKER_CONFIG}/config.json" ]] || printf '{}\n' > "${DOCKER_CONFIG}/config.json"
fi

TAG="${REGISTRY_ADDR}/glue-gateway:dev"
log "Building and pushing ${TAG}..."
docker build --build-arg "TARGETARCH=${GOARCH}" --build-arg "GO_VERSION=${GO_TOOLCHAIN#go}" \
  -t "${TAG}" -f "${REPO_ROOT}/deploy/ax/gateway/Dockerfile" "${REPO_ROOT}" >&2
docker push -q "${TAG}" >/dev/null
GLUE_GATEWAY_IMAGE="$(docker inspect --format='{{index .RepoDigests 0}}' "${TAG}")"
GLUE_SUSPEND_AFTER="${GLUE_SUSPEND_AFTER:-2m}"
export GLUE_GATEWAY_IMAGE GLUE_TASK_IMAGE AX_SYSTEM_NAMESPACE GLUE_SUSPEND_AFTER

{
  [[ -n "${AX_SERVER_IMAGE:-}" ]] && printf 'export AX_SERVER_IMAGE="%s"\n' "${AX_SERVER_IMAGE}"
  [[ -n "${AX_TASK_RUNNER_IMAGE:-}" ]] && printf 'export AX_TASK_RUNNER_IMAGE="%s"\n' "${AX_TASK_RUNNER_IMAGE}"
  [[ -n "${AX_DEFAULT_TASK_IMAGE:-}" ]] && printf 'export AX_DEFAULT_TASK_IMAGE="%s"\n' "${AX_DEFAULT_TASK_IMAGE}"
  printf 'export GLUE_TASK_IMAGE="%s"\nexport GLUE_GATEWAY_IMAGE="%s"\n' "${GLUE_TASK_IMAGE}" "${GLUE_GATEWAY_IMAGE}"
} > "${STATE}/images.env"

umask 077
cat > "${STATE}/gateway.env" <<EOF
export GLUE_GATEWAY_IMAGE="${GLUE_GATEWAY_IMAGE}"
export GLUE_GATEWAY_TOKEN="${GATEWAY_TOKEN}"
EOF

log "Applying glue-system namespace, secret, and manifests..."
k create namespace glue-system --dry-run=client -o yaml | k apply -f - >/dev/null
k -n glue-system create secret generic glue-gateway-secrets \
  --from-literal=GLUE_GATEWAY_TOKEN="${GATEWAY_TOKEN}" \
  --from-literal=GLUE_GITHUB_TOKEN="${GITHUB_TOKEN}" \
  --from-literal=GEMINI_API_KEY="${GEMINI_API_KEY:-}" \
  --from-literal=OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-}" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

# shellcheck disable=SC2016
envsubst '${GLUE_GATEWAY_IMAGE} ${GLUE_TASK_IMAGE} ${AX_SYSTEM_NAMESPACE} ${GLUE_SUSPEND_AFTER}' \
  < "${REPO_ROOT}/deploy/ax/gateway/manifests.yaml" | k apply -f -
k -n glue-system rollout status deployment/glue-gateway --timeout=120s
log "glue-gateway ready (${GLUE_GATEWAY_IMAGE}); token saved in ${STATE}/gateway.env"
