#!/usr/bin/env bash
# scripts/ax-dev/e2e/suspend-resume.sh — verify AX task idle suspend on waiting_for_input,
# read-only registry inspection while suspended, on-demand resume on approve, and suspend on finish.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AX_DEV_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${AX_DEV_DIR}/../.." && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${AX_DEV_DIR}/versions.env"

REPO_URL="${GLUE_E2E_REPO:-https://github.com/erain/glue-sandbox}"
BASE_REF="${GLUE_E2E_BASE:-main}"
OBJECTIVE="${GLUE_E2E_OBJECTIVE:-Add a short paragraph to README.md describing this sandbox repository}"
MODEL="${GLUE_E2E_MODEL:-gemini-3-flash-preview}"
THINKING="${GLUE_E2E_THINKING:-medium}"
SUSPEND_AFTER="${GLUE_SUSPEND_AFTER:-5s}"

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
  GLUE_SUSPEND_AFTER="${SUSPEND_AFTER}" "${AX_DEV_DIR}/deploy-gateway.sh"
else
  k -n glue-system set env deployment/glue-gateway "GLUE_SUSPEND_AFTER=${SUSPEND_AFTER}" >/dev/null
  k -n glue-system rollout status deployment/glue-gateway --timeout=120s >/dev/null
fi
# shellcheck disable=SC1090,SC1091
source "${STATE}/gateway.env"
[[ -n "${GLUE_GATEWAY_TOKEN:-}" ]] || fail "GLUE_GATEWAY_TOKEN missing in ${STATE}/gateway.env"

mkdir -p "${STATE}/bin"
log "Building host glue CLI in ${STATE}/bin/glue..."
(cd "${REPO_ROOT}" && go build -o "${STATE}/bin/glue" ./cmd/glue)
export PATH="${STATE}/bin:${PATH}"

TMP_DIR="$(mktemp -d -t glue-ax-suspend-e2e-XXXXXX)"
GW_PF_PID="" AX_PF_PID="" JOB_ID=""
cleanup() {
  local rc=$?
  set +e
  if [[ "${KEEP_E2E_JOB:-false}" != "true" && -n "${JOB_ID}" ]]; then
    if [[ -n "${GLUE_SERVER:-}" ]]; then
      glue job delete --server "${GLUE_SERVER}" "${JOB_ID}" >/dev/null 2>&1 || true
    fi
    [[ -n "${AX_PORT:-}" ]] && {
      ax --server "http://127.0.0.1:${AX_PORT}" --context "${KUBE_CONTEXT}" -a default delete task "glue-${JOB_ID}" >/dev/null 2>&1 || true
      ax --server "http://127.0.0.1:${AX_PORT}" --context "${KUBE_CONTEXT}" -a default delete workspace "glue-${JOB_ID}-ws" >/dev/null 2>&1 || true
    }
    kubectl ate --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" delete egress-policy "glue-${JOB_ID}" -a default >/dev/null 2>&1 || true
  fi
  [[ -n "${GW_PF_PID}" ]] && kill "${GW_PF_PID}" >/dev/null 2>&1 && wait "${GW_PF_PID}" 2>/dev/null
  [[ -n "${AX_PF_PID}" ]] && kill "${AX_PF_PID}" >/dev/null 2>&1 && wait "${AX_PF_PID}" 2>/dev/null
  ax tunnel stop "${KUBE_CONTEXT}" >/dev/null 2>&1 || true
  rm -rf "${TMP_DIR}"
  exit "${rc}"
}
trap cleanup EXIT

k -n glue-system port-forward svc/glue-gateway 0:80 >"${TMP_DIR}/gw-pf.log" 2>&1 </dev/null &
GW_PF_PID=$!
k -n "${AX_SYSTEM_NAMESPACE}" port-forward svc/ax-server 0:8080 >"${TMP_DIR}/ax-pf.log" 2>&1 </dev/null &
AX_PF_PID=$!

GW_PORT="" AX_PORT=""
for _ in $(seq 1 50); do
  [[ -z "${GW_PORT}" ]] && GW_PORT="$(sed -nE 's/.*127\.0\.0\.1:([0-9]+).*/\1/p' "${TMP_DIR}/gw-pf.log" | head -n1)"
  [[ -z "${AX_PORT}" ]] && AX_PORT="$(sed -nE 's/.*127\.0\.0\.1:([0-9]+).*/\1/p' "${TMP_DIR}/ax-pf.log" | head -n1)"
  [[ -n "${GW_PORT}" && -n "${AX_PORT}" ]] && curl -fsS "http://127.0.0.1:${GW_PORT}/v1/health" >/dev/null 2>&1 && curl -fsS "http://127.0.0.1:${AX_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
[[ -n "${GW_PORT}" && -n "${AX_PORT}" ]] || fail "port-forwards did not become ready"

export GLUE_SERVER="http://127.0.0.1:${GW_PORT}" GLUE_TOKEN="${GLUE_GATEWAY_TOKEN}" GLUE_CONFIG_DIR="${TMP_DIR}/glue-config"
export AX_HOME="${TMP_DIR}/ax-home"
run_ax() { ax --server "http://127.0.0.1:${AX_PORT}" --context "${KUBE_CONTEXT}" -a default "$@"; }
task_phase() { run_ax get task "glue-${JOB_ID}" -o yaml 2>/dev/null | awk '$1 == "phase:" && !p++ {print $2}'; }
pending_perms() { curl -fsS -H "Authorization: Bearer ${GLUE_TOKEN}" "${GLUE_SERVER}/v1/jobs/${JOB_ID}/permissions?state=pending"; }

glue job login --server "${GLUE_SERVER}" --token "${GLUE_TOKEN}" >/dev/null
log "1. Submitting job with --policy ask-all (no live viewer attached)..."
JOB_ID="$(glue job submit --server "${GLUE_SERVER}" --backend ax --repo "${REPO_URL}" --base "${BASE_REF}" \
  --model "${MODEL}" --thinking "${THINKING}" \
  --objective "${OBJECTIVE}" --policy ask-all --deliver branch | head -n1 | tr -d '\r\n')"
[[ "${JOB_ID}" =~ ^job- ]] || fail "Expected job ID, got: ${JOB_ID}"
printf '  [ok] Submitted %s (AX task glue-%s)\n' "${JOB_ID}" "${JOB_ID}"

log "2. Waiting for job to enter waiting_for_input and then suspend after ${SUSPEND_AFTER}..."
PERM_ID=""
for _ in $(seq 1 120); do
  if [[ -z "${PERM_ID}" ]]; then
    PERM_ID="$(pending_perms | jq -r '.permissions[0].id // empty')"
  fi
  DETAIL="$(glue job show --server "${GLUE_SERVER}" "${JOB_ID}" --json)"
  ST="$(jq -r '.status' <<<"${DETAIL}")"
  REASON="$(jq -r '.status_reason // empty' <<<"${DETAIL}")"
  PHASE="$(task_phase)"
  if [[ "${ST}" == "suspended" && "${PHASE}" == "Suspended" && -n "${PERM_ID}" ]]; then
    printf '  [ok] Job %s is suspended (reason=%s, ax phase=%s, pending perm=%s)\n' "${JOB_ID}" "${REASON}" "${PHASE}" "${PERM_ID}"
    break
  fi
  sleep 1
done
[[ -n "${PERM_ID}" && "$(task_phase)" == "Suspended" ]] || fail "Job ${JOB_ID} did not suspend on waiting_for_input"

log "3. Verifying read-only CLI inspection does not wake the suspended AX task..."
glue job list --server "${GLUE_SERVER}" --json >/dev/null
SHOW_JSON="$(glue job show --server "${GLUE_SERVER}" "${JOB_ID}" --json)"
[[ "$(jq -r '.status' <<<"${SHOW_JSON}")" == "suspended" ]] || fail "Expected suspended status in glue job show"
[[ "$(task_phase)" == "Suspended" ]] || fail "Read-only inspection woke AX task glue-${JOB_ID}"
printf '  [ok] glue job list and glue job show left glue-%s in Suspended phase\n' "${JOB_ID}"

log "4. Approving permission ${PERM_ID} to resume the AX task on demand..."
glue job approve --server "${GLUE_SERVER}" "${JOB_ID}" "${PERM_ID}" --remember session --reason "e2e approve after suspend" >/dev/null

log "5. Waiting for resumed job ${JOB_ID} to finish and suspend on finish..."
for _ in $(seq 1 180); do
  DETAIL="$(glue job show --server "${GLUE_SERVER}" "${JOB_ID}" --json)"
  ST="$(jq -r '.status' <<<"${DETAIL}")"
  REASON="$(jq -r '.status_reason // empty' <<<"${DETAIL}")"
  if [[ "${ST}" == "achieved" && "${REASON}" == "finished" && "$(task_phase)" == "Suspended" ]]; then
    break
  fi
  NEXT_PERM="$(pending_perms | jq -r --arg orig "${PERM_ID}" '.permissions[] | select(.id != $orig) | .id' | head -n1)"
  if [[ -n "${NEXT_PERM}" ]]; then
    glue job approve --server "${GLUE_SERVER}" "${JOB_ID}" "${NEXT_PERM}" --remember session --reason "auto-approve follow-up" >/dev/null 2>&1 || true
  fi
  sleep 1
done

RESULT_JSON="$(glue job result --server "${GLUE_SERVER}" "${JOB_ID}" --json)"
jq -e '.status == "achieved"' <<<"${RESULT_JSON}" >/dev/null || fail "Expected achieved result, got: ${RESULT_JSON}"
[[ "$(task_phase)" == "Suspended" ]] || fail "Expected AX task glue-${JOB_ID} to remain Suspended after result fetch"

EVENTS_RAW="$(curl -fsS -H "Authorization: Bearer ${GLUE_TOKEN}" "${GLUE_SERVER}/v1/jobs/${JOB_ID}/events?cached=1")"
EVENTS_JSON="$(sed -n 's/^data: //p' <<<"${EVENTS_RAW}" | jq -cs '.')"
SEQ_OK="$(jq -r '(length >= 6) and (map(.seq) == [range(1; length + 1)])' <<<"${EVENTS_JSON}")"
[[ "${SEQ_OK}" == "true" ]] || fail "Expected continuous event sequence 1..N, got: $(jq -c 'map(.seq)' <<<"${EVENTS_JSON}")"
for want in task_suspended task_resumed job_resumed permission_decision job_done; do
  jq -e --arg t "${want}" 'any(.[]; .type == $t)' <<<"${EVENTS_JSON}" >/dev/null || fail "Missing event ${want}"
done
jq -e --arg orig "${PERM_ID}" 'any(.[]; .type == "permission_decision" and .payload.original_permission_id == $orig and .payload.permission_id != $orig)' <<<"${EVENTS_JSON}" >/dev/null || fail "Expected permission_decision with original_permission_id=${PERM_ID}"

SEQ_COUNT="$(jq -r 'length' <<<"${EVENTS_JSON}")"
printf '\n[PASS] job_id=%s status=achieved ax_phase=Suspended orig_perm=%s events=1..%s\n' "${JOB_ID}" "${PERM_ID}" "${SEQ_COUNT}"
