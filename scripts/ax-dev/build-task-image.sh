#!/usr/bin/env bash
# scripts/ax-dev/build-task-image.sh — build and push the Glue AX task image to localhost:5001/glue-task.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

log() { printf '==> %s\n' "$*" >&2; }
export GOARCH
GOARCH="$(go env GOARCH)"
mkdir -p "${STATE}/src" "${STATE}/docker-config"
if [[ -z "${DOCKER_CONFIG:-}" ]]; then
  export DOCKER_CONFIG="${STATE}/docker-config"
  [[ -f "${DOCKER_CONFIG}/config.json" ]] || printf '{}\n' > "${DOCKER_CONFIG}/config.json"
fi

if [[ "$(docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null || true)" != "true" ]]; then
  docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  docker run -d --restart=always --label created-by=agent-substrate \
    -p "127.0.0.1:${REGISTRY_PORT}:5000" -p "[::1]:${REGISTRY_PORT}:5000" \
    --network bridge --name "${REGISTRY_NAME}" registry:3 >/dev/null
fi
if docker network inspect kind >/dev/null 2>&1 &&
  [[ "$(docker inspect -f '{{json .NetworkSettings.Networks.kind}}' "${REGISTRY_NAME}" 2>/dev/null || echo null)" == "null" ]]; then
  docker network connect kind "${REGISTRY_NAME}" 2>/dev/null || true
fi

[[ -f "${STATE}/images.env" ]] && { # shellcheck disable=SC1090,SC1091
  source "${STATE}/images.env"; }

RUNNER_TAG="${REGISTRY_ADDR}/ax-task-runner:dev"
if [[ -z "${AX_TASK_RUNNER_IMAGE:-}" ]] || ! docker image inspect "${AX_TASK_RUNNER_IMAGE}" >/dev/null 2>&1; then
  if ! docker image inspect "${RUNNER_TAG}" >/dev/null 2>&1; then
    AX_DIR="${STATE}/src/ax"
    [[ -d "${AX_DIR}/.git" ]] || { git clone --quiet --reference-if-able /home/ubuntu/src/ax "${AX_REPO}" "${AX_DIR}" && git -C "${AX_DIR}" checkout --force --quiet "${AX_COMMIT}"; }
    (cd "${AX_DIR}" && mkdir -p "bin/linux_${GOARCH}" && CGO_ENABLED=0 GOOS=linux go build -o "bin/linux_${GOARCH}/ax-task-runner" ./cmd/ax-task-runner)
    docker build -q -t "${RUNNER_TAG}" --build-arg TARGETARCH="${GOARCH}" -f "${AX_DIR}/Dockerfile.task-runner" "${AX_DIR}" >/dev/null
  fi
  docker push -q "${RUNNER_TAG}" >/dev/null
  AX_TASK_RUNNER_IMAGE="$(docker inspect --format='{{index .RepoDigests 0}}' "${RUNNER_TAG}")"
else
  docker push -q "${RUNNER_TAG}" >/dev/null 2>&1 || true
fi

TAG="${REGISTRY_ADDR}/glue-task:dev"
build_args=(--build-arg "BASE_IMAGE=${AX_TASK_RUNNER_IMAGE}" --build-arg "TARGETARCH=${GOARCH}" --build-arg "GO_VERSION=${GO_TOOLCHAIN#go}")
[[ -n "${PREWARM_GOMOD_REPO:-}" ]] && build_args+=(--build-arg "PREWARM_GOMOD_REPO=${PREWARM_GOMOD_REPO}")
[[ -n "${PREWARM_GOMOD_REF:-}" ]] && build_args+=(--build-arg "PREWARM_GOMOD_REF=${PREWARM_GOMOD_REF}")

log "Building ${TAG} from ${AX_TASK_RUNNER_IMAGE}..."
docker build "${build_args[@]}" -t "${TAG}" -f "${REPO_ROOT}/deploy/ax/task-image/Dockerfile" "${REPO_ROOT}" >&2
docker push -q "${TAG}" >/dev/null
GLUE_TASK_IMAGE="$(docker inspect --format='{{index .RepoDigests 0}}' "${TAG}")"
IMG_BYTES="$(docker image inspect --format='{{.Size}}' "${TAG}")"

{
  [[ -n "${AX_SERVER_IMAGE:-}" ]] && printf 'export AX_SERVER_IMAGE="%s"\n' "${AX_SERVER_IMAGE}"
  printf 'export AX_TASK_RUNNER_IMAGE="%s"\nexport AX_DEFAULT_TASK_IMAGE="%s"\nexport GLUE_TASK_IMAGE="%s"\n' \
    "${AX_TASK_RUNNER_IMAGE}" "${AX_DEFAULT_TASK_IMAGE:-${AX_TASK_RUNNER_IMAGE}}" "${GLUE_TASK_IMAGE}"
} > "${STATE}/images.env"

log "Pushed ${GLUE_TASK_IMAGE} ($(( IMG_BYTES / 1024 / 1024 )) MiB / ${IMG_BYTES} bytes); recorded in ${STATE}/images.env"
printf '%s\n' "${GLUE_TASK_IMAGE}"
