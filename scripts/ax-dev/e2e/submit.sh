#!/usr/bin/env bash
# scripts/ax-dev/e2e/submit.sh — submit an AX job through glue-gateway on kind and verify delivery + SSE continuity.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AX_DEV_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${AX_DEV_DIR}/../.." && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${AX_DEV_DIR}/versions.env"

REPO_URL="${GLUE_E2E_REPO:-https://github.com/erain/glue-sandbox}"
BASE_REF="${GLUE_E2E_BASE:-main}"
OBJECTIVE="${GLUE_E2E_OBJECTIVE:-Create go.mod (module example.com/sandbox, go 1.24), greet.go (package sandbox; func Greet(name string) string returning \"Hello, \" + name + \"!\"), and greet_test.go (package sandbox; func TestGreet(t *testing.T)), then run go test ./...}"
PORT="${GLUE_GATEWAY_PORT:-0}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) REPO_URL="$2"; shift 2 ;;
    --base) BASE_REF="$2"; shift 2 ;;
    --objective) OBJECTIVE="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

log() { printf '\n==> %s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"; }

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  "${AX_DEV_DIR}/up.sh"
fi
if [[ ! -f "${STATE}/images.env" ]] || ! grep -q '^export GLUE_TASK_IMAGE=' "${STATE}/images.env"; then
  "${AX_DEV_DIR}/build-task-image.sh"
fi
if [[ ! -f "${STATE}/gateway.env" ]] || ! k -n glue-system get deployment glue-gateway >/dev/null 2>&1; then
  "${AX_DEV_DIR}/deploy-gateway.sh"
fi
# shellcheck disable=SC1090,SC1091
source "${STATE}/gateway.env"
[[ -n "${GLUE_GATEWAY_TOKEN:-}" ]] || fail "GLUE_GATEWAY_TOKEN missing in ${STATE}/gateway.env"

mkdir -p "${STATE}/bin"
log "Building host glue CLI in ${STATE}/bin/glue..."
(cd "${REPO_ROOT}" && go build -o "${STATE}/bin/glue" ./cmd/glue)
export PATH="${STATE}/bin:${PATH}"

TMP_DIR="$(mktemp -d -t glue-ax-e2e-XXXXXX)"
PF_PID=""
JOB_ID=""
cleanup() {
  local rc=$?
  set +e
  if [[ "${KEEP_E2E_JOB:-false}" != "true" && -n "${JOB_ID}" && -n "${GLUE_SERVER:-}" ]]; then
    glue job delete --server "${GLUE_SERVER}" "${JOB_ID}" >/dev/null 2>&1 || true
  fi
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" >/dev/null 2>&1 && wait "${PF_PID}" 2>/dev/null
  rm -rf "${TMP_DIR}"
  exit "${rc}"
}
trap cleanup EXIT

k -n glue-system port-forward svc/glue-gateway "${PORT}:80" >"${TMP_DIR}/pf.log" 2>&1 </dev/null &
PF_PID=$!
GW_PORT=""
for _ in $(seq 1 50); do
  GW_PORT="$(sed -nE 's/.*127\.0\.0\.1:([0-9]+).*/\1/p' "${TMP_DIR}/pf.log" | head -n1)"
  [[ -n "${GW_PORT}" ]] && curl -fsS "http://127.0.0.1:${GW_PORT}/v1/health" >/dev/null 2>&1 && break
  sleep 0.2
done
[[ -n "${GW_PORT}" ]] || fail "glue-gateway port-forward did not become ready"
export GLUE_SERVER="http://127.0.0.1:${GW_PORT}" GLUE_TOKEN="${GLUE_GATEWAY_TOKEN}"
export GLUE_CONFIG_DIR="${TMP_DIR}/glue-config"

glue job login --server "${GLUE_SERVER}" --token "${GLUE_TOKEN}" >/dev/null
log "Submitting job via glue-gateway (${GLUE_SERVER}, backend=ax, repo=${REPO_URL})..."
glue job submit --server "${GLUE_SERVER}" --backend ax \
  --repo "${REPO_URL}" --base "${BASE_REF}" \
  --objective "${OBJECTIVE}" \
  --deliver draft_pr --verify "go test ./..." \
  --attach </dev/null | tee "${TMP_DIR}/submit.out"

JOB_ID="$(head -n1 "${TMP_DIR}/submit.out" | tr -d '\r\n')"
[[ "${JOB_ID}" =~ ^job- ]] || fail "Expected job ID on first line of submit output, got: ${JOB_ID}"

log "Fetching result and verifying assertions for ${JOB_ID}..."
RESULT_JSON="$(glue job result --server "${GLUE_SERVER}" "${JOB_ID}" --json)"
printf '%s\n' "${RESULT_JSON}" | jq '{job_id, status, branch, base_sha, head_sha, pr_url, delivery_error, verify}'

jq -e '.status == "achieved"' <<<"${RESULT_JSON}" >/dev/null || fail "Expected status == achieved"
PR_URL="$(jq -r '.pr_url // empty' <<<"${RESULT_JSON}")"
[[ -n "${PR_URL}" ]] || fail "Expected non-empty pr_url in result"

EVENTS_RAW="$(curl -fsS -H "Authorization: Bearer ${GLUE_TOKEN}" "${GLUE_SERVER}/v1/jobs/${JOB_ID}/events")"
SEQS="$(sed -n 's/^data: //p' <<<"${EVENTS_RAW}" | jq -cs 'map(.seq)')"
SEQ_OK="$(jq -r '(length >= 2) and (. == [range(1; length + 1)])' <<<"${SEQS}")"
[[ "${SEQ_OK}" == "true" ]] || fail "Expected continuous event sequence 1..N, got: ${SEQS}"
SEQ_COUNT="$(jq -r 'length' <<<"${SEQS}")"

printf '\n[PASS] job_id=%s status=achieved pr_url=%s events=1..%s\n' "${JOB_ID}" "${PR_URL}" "${SEQ_COUNT}"
