#!/usr/bin/env bash
# scripts/ax-dev/up.sh — provision the pinned kind + Substrate + AX cluster (idempotent).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

log() { printf '\n==> %s\n' "$*" >&2; }
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"; }

"${SCRIPT_DIR}/prereqs.sh"

SUB_DIR="${STATE}/src/substrate"
AX_DIR="${STATE}/src/ax"
E2E_DIR="${AX_DIR}/test/e2e"
export GOARCH
GOARCH="$(go env GOARCH)"
mkdir -p "${STATE}/bin" "${STATE}/src" "${STATE}/docker-config"
if [[ -z "${DOCKER_CONFIG:-}" ]]; then
  export DOCKER_CONFIG="${STATE}/docker-config"
  [[ -f "${DOCKER_CONFIG}/config.json" ]] || printf '{}\n' > "${DOCKER_CONFIG}/config.json"
fi

ensure_checkout() {
  local dir="$1" repo="$2" ref="$3" sha="$4" seed="$5"
  if [[ ! -d "${dir}/.git" ]]; then
    log "Cloning ${repo} into ${dir}..."
    git clone --quiet --reference-if-able "${seed}" "${repo}" "${dir}"
  fi
  if ! git -C "${dir}" rev-parse --verify --quiet "${sha}^{commit}" >/dev/null 2>&1 ||
    ! git -C "${dir}" rev-parse --verify --quiet "${ref}^{commit}" >/dev/null 2>&1; then
    if [[ -d "${seed}/.git" ]]; then git -C "${dir}" fetch --quiet --tags --force "${seed}" 2>/dev/null || true; fi
    git -C "${dir}" rev-parse --verify --quiet "${ref}^{commit}" >/dev/null 2>&1 || git -C "${dir}" fetch --quiet --tags --force origin
  fi
  if [[ "$(git -C "${dir}" rev-parse HEAD)" != "${sha}" ]] || [[ -n "$(git -C "${dir}" status --porcelain --untracked-files=no)" ]]; then
    git -C "${dir}" checkout --force --quiet "${ref}"
    git -C "${dir}" clean -fdq
  fi
  [[ "$(git -C "${dir}" rev-parse HEAD)" == "${sha}" ]] || { printf 'ERROR: %s HEAD != %s\n' "${dir}" "${sha}" >&2; exit 1; }
}

RELEASE_TAG_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'
if [[ ! "${SUBSTRATE_TAG}" =~ ${RELEASE_TAG_RE} ]]; then
  printf 'ERROR: SUBSTRATE_TAG=%s is not a release tag (vX.Y.Z or vX.Y.Z-rc.N)\n' "${SUBSTRATE_TAG}" >&2
  exit 1
fi

ensure_checkout "${SUB_DIR}" "${SUBSTRATE_REPO}" "${SUBSTRATE_TAG}" "${SUBSTRATE_COMMIT}" "/home/ubuntu/src/agent-substrate"
if ! git -C "${SUB_DIR}" tag --points-at HEAD | grep -qx "${SUBSTRATE_TAG}"; then
  printf 'ERROR: Substrate checkout %s is not at tag %s\n' "${SUB_DIR}" "${SUBSTRATE_TAG}" >&2
  exit 1
fi
[[ -d "/home/ubuntu/src/agent-substrate/.cache" && ! -e "${SUB_DIR}/.cache" ]] && ln -s "/home/ubuntu/src/agent-substrate/.cache" "${SUB_DIR}/.cache"
ensure_checkout "${AX_DIR}" "${AX_REPO}" "${AX_COMMIT}" "${AX_COMMIT}" "/home/ubuntu/src/ax"

if [[ ! -x "${STATE}/bin/kind" ]] || [[ "$("${STATE}/bin/kind" --version 2>/dev/null || true)" != *"${KIND_VERSION#v}"* ]]; then
  log "Building kind ${KIND_VERSION} from Substrate tools..."
  ln -sf "$(cd "${SUB_DIR}" && ./hack/run-tool.sh --print-bin-path kind)" "${STATE}/bin/kind"
fi
if [[ ! -x "${STATE}/bin/kubectl-ate" ]] || [[ "$(cat "${STATE}/bin/.substrate-sha" 2>/dev/null || true)" != "${SUBSTRATE_COMMIT}" ]]; then
  log "Building kubectl-ate (${SUBSTRATE_TAG})..."
  (cd "${SUB_DIR}" && CGO_ENABLED=0 go build -o "${STATE}/bin/kubectl-ate" ./cmd/kubectl-ate)
  printf '%s\n' "${SUBSTRATE_COMMIT}" > "${STATE}/bin/.substrate-sha"
fi
if [[ ! -x "${STATE}/bin/ax" ]] || [[ "$(cat "${STATE}/bin/.ax-sha" 2>/dev/null || true)" != "${AX_COMMIT}" ]]; then
  log "Building ax CLI (${AX_COMMIT:0:12})..."
  (cd "${AX_DIR}" && CGO_ENABLED=0 go build -o "${STATE}/bin/ax" ./cmd/ax)
  printf '%s\n' "${AX_COMMIT}" > "${STATE}/bin/.ax-sha"
fi

if [[ "${RECREATE_KIND:-false}" == "true" ]] || ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  log "Creating kind cluster '${KIND_CLUSTER_NAME}' and registry '${REGISTRY_ADDR}'..."
  if (( $(sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0) < 8192 )); then
    sudo -n sysctl -w fs.inotify.max_user_instances=8192 >/dev/null 2>&1 || true
  fi
  (cd "${SUB_DIR}" && KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" KIND_REGISTRY_PORT="${REGISTRY_PORT}" ./hack/create-kind-cluster.sh)
else
  log "Reusing kind cluster '${KIND_CLUSTER_NAME}'..."
  if [[ "$(docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null || true)" != "true" ]]; then
    docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
    docker run -d --restart=always --label created-by=agent-substrate \
      -p "127.0.0.1:${REGISTRY_PORT}:5000" -p "[::1]:${REGISTRY_PORT}:5000" \
      --network bridge --name "${REGISTRY_NAME}" registry:3 >/dev/null
  fi
  if [[ "$(docker inspect -f '{{json .NetworkSettings.Networks.kind}}' "${REGISTRY_NAME}" 2>/dev/null || echo null)" == "null" ]]; then
    docker network connect kind "${REGISTRY_NAME}" 2>/dev/null || true
  fi
fi
kind export kubeconfig --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" >/dev/null
for node in $(kind get nodes --name "${KIND_CLUSTER_NAME}"); do
  docker exec "${node}" sysctl -w fs.inotify.max_user_instances=8192 >/dev/null
done

image_digest() {
  local tag="$2" host="${1%%/*}" path="${1#*/}" headers challenge realm service token
  local accept=(-H 'Accept: application/vnd.oci.image.index.v1+json' -H 'Accept: application/vnd.oci.image.manifest.v1+json' -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' -H 'Accept: application/vnd.docker.distribution.manifest.v2+json')
  headers="$(curl -sSI "${accept[@]}" "https://${host}/v2/${path}/manifests/${tag}" | tr -d '\r')"
  if [[ "$(awk 'NR==1 {print $2}' <<<"${headers}")" == "401" ]]; then
    challenge="$(awk 'tolower($1) == "www-authenticate:"' <<<"${headers}")"
    realm="$(sed -nE 's/.*realm="([^"]+)".*/\1/p' <<<"${challenge}")"
    service="$(sed -nE 's/.*service="([^"]+)".*/\1/p' <<<"${challenge}")"
    token="$(curl -fsS -G "${realm}" --data-urlencode "service=${service}" --data-urlencode "scope=repository:${path}:pull" | sed -nE 's/.*"(access_)?token" *: *"([^"]+)".*/\2/p')"
    headers="$(curl -fsSI "${accept[@]}" -H "Authorization: Bearer ${token}" "https://${host}/v2/${path}/manifests/${tag}" | tr -d '\r')"
  fi
  awk 'tolower($1) == "docker-content-digest:" {print $2; found=1} END {exit !found}' <<<"${headers}"
}

uninstall_substrate() {
  k -n "${AX_SYSTEM_NAMESPACE}" delete workerpool ax-workers --ignore-not-found --timeout=30s >/dev/null 2>&1 ||
    k -n "${AX_SYSTEM_NAMESPACE}" patch workerpool ax-workers --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  k delete validatingadmissionpolicybinding,validatingadmissionpolicy -l app.kubernetes.io/part-of=ate --ignore-not-found >/dev/null 2>&1 || true
  k delete validatingadmissionpolicybinding,validatingadmissionpolicy sandboxconfig-assets --ignore-not-found >/dev/null 2>&1 || true
  if ! k delete sandboxconfig --all --ignore-not-found --timeout=30s >/dev/null 2>&1; then
    local configs
    configs="$(k get sandboxconfig -o name 2>/dev/null || true)"
    if [[ -n "${configs}" ]]; then
      xargs kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" patch --type=merge -p '{"metadata":{"finalizers":null}}' <<<"${configs}" >/dev/null 2>&1 || true
    fi
  fi
  local crds
  crds="$(k get crd -o name 2>/dev/null | grep '\.ate\.dev$' || true)"
  if [[ -n "${crds}" ]]; then
    xargs kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" delete --ignore-not-found <<<"${crds}" >/dev/null 2>&1 || true
  fi
  k delete namespace ate-system --ignore-not-found --wait=true >/dev/null 2>&1 || true
  k label nodes --all ate.dev/substrate-version- >/dev/null 2>&1 || true
}

WORKER_KO_REF="$(awk '$1 == "workerImage:" {print $2; exit}' "${E2E_DIR}/manifests/workerpool.yaml")"
WORKER_IMG="${WORKER_KO_REF##*/}"
SUBSTRATE_IMAGES=(ateapi atecontroller atelet atenet "${WORKER_IMG}" podcertcontroller objectstore-plugin envoy-dataplane)

INGRESS_SPIFFE_IDS="spiffe://cluster.local/ns/${AX_SYSTEM_NAMESPACE}/sa/ax-server,spiffe://cluster.local/ns/ate-system/sa/atenet-client"
BOOTSTRAP_OWNERS="system:serviceaccount:ate-system:ate-client,system:serviceaccount:${AX_SYSTEM_NAMESPACE}:ax-server"
SUB_CFG_SPEC="${SUBSTRATE_COMMIT}:${INGRESS_SPIFFE_IDS}:k8s.io"
INSTALLED_SUB="$(k get namespace ate-system -o jsonpath='{.metadata.annotations.glue\.ax-dev/spec}' 2>/dev/null || true)"

if [[ "${FORCE_INSTALL_SUBSTRATE:-false}" == "true" ]] || [[ "${INSTALLED_SUB}" != "${SUB_CFG_SPEC}" ]] ||
  ! k -n ate-system get deployment ate-api-server >/dev/null 2>&1; then
  grep -q -- "--image-repo" "${SUB_DIR}/cmd/ate-setup/commands.md" 2>/dev/null ||
    { printf 'ERROR: Substrate %s ate-setup lacks --image-repo\n' "${SUBSTRATE_TAG}" >&2; exit 1; }
  for img in "${SUBSTRATE_IMAGES[@]}"; do
    image_digest "${SUBSTRATE_IMAGE_REPO}/${img}" "${SUBSTRATE_TAG}" >/dev/null ||
      { printf 'ERROR: release image %s/%s:%s not found\n' "${SUBSTRATE_IMAGE_REPO}" "${img}" "${SUBSTRATE_TAG}" >&2; exit 1; }
  done
  if [[ -n "${INSTALLED_SUB}" ]] || k get namespace ate-system >/dev/null 2>&1; then
    log "Removing previous Substrate installation before deploying ${SUBSTRATE_TAG}..."
    uninstall_substrate
  fi
  log "Installing Agent Substrate ${SUBSTRATE_TAG} (${SUBSTRATE_COMMIT:0:12}) from ${SUBSTRATE_IMAGE_REPO}..."
  (
    cd "${SUB_DIR}"
    ATE_IMAGE_REPO="${SUBSTRATE_IMAGE_REPO}" ATE_IMAGE_TAG="${SUBSTRATE_TAG}" \
      go run ./cmd/ate-setup --kind --kind-cluster-name="${KIND_CLUSTER_NAME}" \
      --kubeconfig="${KUBECONFIG}" --context="${KUBE_CONTEXT}" --no-dev-env \
      --image-repo="${SUBSTRATE_IMAGE_REPO}" --image-tag="${SUBSTRATE_TAG}" \
      deploy ate-system \
      --rollout-timeout="${SUBSTRATE_ROLLOUT_TIMEOUT:-5m}" \
      --credential-provider='{"name":"k8s.io"}' \
      --ateapi-authz-bootstrap-owners="${BOOTSTRAP_OWNERS}" \
      --ingress-allowed-spiffe-ids="${INGRESS_SPIFFE_IDS}"
  )
  k annotate namespace ate-system "substrate-commit=${SUBSTRATE_COMMIT}" "glue.ax-dev/spec=${SUB_CFG_SPEC}" --overwrite >/dev/null
else
  log "Agent Substrate ${SUBSTRATE_TAG} already installed; verifying readiness..."
  k -n ate-system wait --for=condition=Available deployment/ate-api-server --timeout=120s >/dev/null
fi

AX_CFG_SPEC="${AX_COMMIT}:workers=${GLUE_AX_WORKERS}"
INSTALLED_AX="$(k get namespace "${AX_SYSTEM_NAMESPACE}" -o jsonpath='{.metadata.annotations.glue\.ax-dev/spec}' 2>/dev/null || true)"
if [[ "${FORCE_INSTALL_AX:-false}" != "true" ]] && [[ "${INSTALLED_AX}" == "${AX_CFG_SPEC}" ]] &&
  [[ -f "${STATE}/images.env" ]] &&
  k -n "${AX_SYSTEM_NAMESPACE}" get deployment ax-workers ax-server >/dev/null 2>&1 &&
  k -n "${AX_SYSTEM_NAMESPACE}" get statefulset ax-postgres >/dev/null 2>&1; then
  log "AX ${AX_COMMIT:0:12} and ax-workers (${GLUE_AX_WORKERS}) already deployed; verifying readiness..."
  k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-workers --timeout=120s >/dev/null
  k -n "${AX_SYSTEM_NAMESPACE}" rollout status statefulset/ax-postgres --timeout=120s >/dev/null
  k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-server --timeout=120s >/dev/null
  log "Cluster '${KIND_CLUSTER_NAME}' is up to date (no changes needed)."
  exit 0
fi

log "Building and pushing ax-server and ax-task-runner (${AX_COMMIT:0:12}) to ${REGISTRY_ADDR}..."
(
  cd "${AX_DIR}"
  mkdir -p "bin/linux_${GOARCH}"
  CGO_ENABLED=0 GOOS=linux go build -o "bin/linux_${GOARCH}/ax-server" ./cmd/ax-server
  CGO_ENABLED=0 GOOS=linux go build -o "bin/linux_${GOARCH}/ax-task-runner" ./cmd/ax-task-runner
)
build_and_push() {
  local tag="$1"; shift
  docker build -q -t "${tag}" "$@" "${AX_DIR}" >/dev/null
  docker push -q "${tag}" >/dev/null
  docker inspect --format='{{index .RepoDigests 0}}' "${tag}"
}
AX_SERVER_IMAGE="$(build_and_push "${REGISTRY_ADDR}/ax-server:dev" --build-arg TARGETARCH="${GOARCH}" -f "${AX_DIR}/Dockerfile.server")"
AX_TASK_RUNNER_IMAGE="$(build_and_push "${REGISTRY_ADDR}/ax-task-runner:dev" --build-arg TARGETARCH="${GOARCH}" -f "${AX_DIR}/Dockerfile.task-runner")"
export AX_SERVER_IMAGE AX_TASK_RUNNER_IMAGE AX_DEFAULT_TASK_IMAGE="${AX_TASK_RUNNER_IMAGE}" AX_WORKER_REPLICAS="${GLUE_AX_WORKERS}"

EXISTING_TASK_IMAGE=""
if [[ -f "${STATE}/images.env" ]]; then
  EXISTING_TASK_IMAGE="$(sed -nE 's/^export GLUE_TASK_IMAGE="(.*)"$/\1/p' "${STATE}/images.env" | tail -n1)"
fi
{
  printf 'export AX_SERVER_IMAGE="%s"\n' "${AX_SERVER_IMAGE}"
  printf 'export AX_TASK_RUNNER_IMAGE="%s"\n' "${AX_TASK_RUNNER_IMAGE}"
  printf 'export AX_DEFAULT_TASK_IMAGE="%s"\n' "${AX_DEFAULT_TASK_IMAGE}"
  [[ -n "${EXISTING_TASK_IMAGE}" ]] && printf 'export GLUE_TASK_IMAGE="%s"\n' "${EXISTING_TASK_IMAGE}"
} > "${STATE}/images.env"

render_manifest() {
  # shellcheck disable=SC2016
  envsubst '${AX_SYSTEM_NAMESPACE} ${AX_WORKER_REPLICAS} ${AX_SERVER_IMAGE} ${AX_DEFAULT_TASK_IMAGE}' < "${E2E_DIR}/manifests/$1"
}

log "Deploying '${AX_SYSTEM_NAMESPACE}' and WorkerPool 'ax-workers' (${GLUE_AX_WORKERS} gVisor replicas)..."
render_manifest namespace.yaml | k apply -f -
worker_digest="$(image_digest "${SUBSTRATE_IMAGE_REPO}/${WORKER_IMG}" "${SUBSTRATE_TAG}")"
render_manifest workerpool.yaml | sed "s|${WORKER_KO_REF}|${SUBSTRATE_IMAGE_REPO}/${WORKER_IMG}:${SUBSTRATE_TAG}@${worker_digest}|" | k apply -f -
for _ in $(seq 1 60); do k -n "${AX_SYSTEM_NAMESPACE}" get deployment ax-workers >/dev/null 2>&1 && break; sleep 2; done
k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-workers --timeout=180s

log "Deploying ax-postgres and ax-server in '${AX_SYSTEM_NAMESPACE}'..."
if ! k -n "${AX_SYSTEM_NAMESPACE}" get secret ax-postgres >/dev/null 2>&1; then
  k -n "${AX_SYSTEM_NAMESPACE}" create secret generic ax-postgres --from-literal=password="$(openssl rand -hex 24)" --dry-run=client -o yaml | k apply -f -
fi
render_manifest postgres.yaml | k apply -f -
k -n "${AX_SYSTEM_NAMESPACE}" rollout status statefulset/ax-postgres --timeout=180s
render_manifest ax-server.yaml | k apply -f -
k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-server --timeout=120s
k annotate namespace "${AX_SYSTEM_NAMESPACE}" "glue.ax-dev/spec=${AX_CFG_SPEC}" --overwrite >/dev/null
log "Cluster '${KIND_CLUSTER_NAME}' is ready (digests recorded in ${STATE}/images.env)."
