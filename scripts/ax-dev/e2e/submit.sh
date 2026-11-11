#!/usr/bin/env bash
# scripts/ax-dev/e2e/submit.sh — submit a coding job directly to AX via glue CLI and verify draft PR delivery.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AX_DEV_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${AX_DEV_DIR}/../.." && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${AX_DEV_DIR}/versions.env"

REPO_URL="${GLUE_E2E_REPO:-https://github.com/erain/glue-sandbox}"
BASE_REF="${GLUE_E2E_BASE:-main}"
OBJECTIVE="${GLUE_E2E_OBJECTIVE:-Create go.mod (module example.com/sandbox, go 1.24), greet.go (package sandbox; func Greet(name string) string returning \"Hello, \" + name + \"!\"), and greet_test.go (package sandbox; func TestGreet(t *testing.T)), then run go test ./...}"
MODEL="${GLUE_E2E_MODEL:-gemini-3-flash-preview}"
THINKING="${GLUE_E2E_THINKING:-medium}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) REPO_URL="$2"; shift 2 ;;
    --base) BASE_REF="$2"; shift 2 ;;
    --objective) OBJECTIVE="$2"; shift 2 ;;
    --model) MODEL="$2"; shift 2 ;;
    --thinking) THINKING="$2"; shift 2 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

log() { printf '\n==> %s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

if [[ -z "${GEMINI_API_KEY:-}" && -z "${OPENROUTER_API_KEY:-}" && -f "${HOME}/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "${HOME}/.env"
  set +a
fi
[[ -n "${GEMINI_API_KEY:-}" || -n "${OPENROUTER_API_KEY:-}" ]] || fail "GEMINI_API_KEY or OPENROUTER_API_KEY must be set"
export GLUE_GITHUB_TOKEN="${GLUE_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"
if [[ -z "${GLUE_GITHUB_TOKEN}" ]] && command -v gh >/dev/null 2>&1; then
  GLUE_GITHUB_TOKEN="$(gh auth token 2>/dev/null || true)"
fi
[[ -n "${GLUE_GITHUB_TOKEN}" ]] || fail "GLUE_GITHUB_TOKEN, GITHUB_TOKEN, or 'gh auth token' is required"

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  "${AX_DEV_DIR}/up.sh"
fi
if [[ ! -f "${STATE}/images.env" ]] || ! grep -q '^export GLUE_TASK_IMAGE=' "${STATE}/images.env"; then
  "${AX_DEV_DIR}/build-task-image.sh"
fi
# shellcheck disable=SC1090,SC1091
source "${STATE}/images.env"
[[ -n "${GLUE_TASK_IMAGE:-}" ]] || fail "GLUE_TASK_IMAGE missing in ${STATE}/images.env"

mkdir -p "${STATE}/bin"
log "Building host glue CLI in ${STATE}/bin/glue..."
(cd "${REPO_ROOT}" && go build -o "${STATE}/bin/glue" ./cmd/glue)
export PATH="${STATE}/bin:${PATH}"

TMP_DIR="$(mktemp -d -t glue-ax-e2e-XXXXXX)"
chmod 0700 "${TMP_DIR}"
export GLUE_CONFIG_DIR="${TMP_DIR}/glue-config"
unset GLUE_SERVER GLUE_TOKEN

JOB_ID="" PR_URL=""
cleanup() {
  local rc=$?
  set +e
  if [[ "${KEEP_E2E_JOB:-false}" != "true" ]]; then
    if [[ -n "${PR_URL}" ]] && command -v gh >/dev/null 2>&1; then
      GH_TOKEN="${GLUE_GITHUB_TOKEN}" gh pr close "${PR_URL}" --delete-branch >/dev/null 2>&1 || true
    fi
    if [[ -n "${JOB_ID}" ]]; then
      if [[ "${REPO_URL}" =~ github\.com[:/]([^/]+/[^/.]+) ]] && command -v gh >/dev/null 2>&1; then
        GH_TOKEN="${GLUE_GITHUB_TOKEN}" gh api -X DELETE "repos/${BASH_REMATCH[1]}/git/refs/heads/glue/${JOB_ID}" >/dev/null 2>&1 || true
      fi
      glue job delete --ax --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "${JOB_ID}" >/dev/null 2>&1 || true
    fi
  fi
  for tf in "${GLUE_CONFIG_DIR}/tunnels/"*-tunnel.json; do
    [[ -f "${tf}" ]] || continue
    tpid="$(jq -r '.PID // empty' "${tf}" 2>/dev/null)"
    [[ -n "${tpid}" ]] && kill "${tpid}" >/dev/null 2>&1 || true
  done
  rm -rf "${TMP_DIR}"
  exit "${rc}"
}
trap cleanup EXIT

MODEL_ARGS=(--provider openrouter)
if [[ -n "${GEMINI_API_KEY:-}" ]]; then
  MODEL_ARGS=(--provider gemini --model "${MODEL}" --thinking "${THINKING}")
fi

log "Submitting job directly to AX (context=${KUBE_CONTEXT}, repo=${REPO_URL})..."
glue --remote "${OBJECTIVE}" \
  --repo "${REPO_URL}" --base "${BASE_REF}" \
  --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" \
  --image "${GLUE_TASK_IMAGE}" \
  "${MODEL_ARGS[@]}" \
  --deliver draft_pr --verify "go test ./..." \
  --attach </dev/null | tee "${TMP_DIR}/submit.out"

JOB_ID="$(head -n1 "${TMP_DIR}/submit.out" | tr -d '\r\n')"
[[ "${JOB_ID}" =~ ^job- ]] || fail "Expected job ID on first line of submit output, got: ${JOB_ID}"

log "Inspecting job and verifying completion result for ${JOB_ID}..."
SHOW_JSON="$(glue job show --ax --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "${JOB_ID}" --json)"
LAST_SEQ="$(jq -r '.last_seq // 0' <<<"${SHOW_JSON}")"
(( LAST_SEQ >= 2 )) || fail "Expected last_seq >= 2 in glue job show, got: ${LAST_SEQ}"

RESULT_JSON="$(glue job result --ax --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "${JOB_ID}" --json)"
PR_URL="$(jq -r '.pr_url // empty' <<<"${RESULT_JSON}")"
printf '%s\n' "${RESULT_JSON}" | jq '{job_id, status, branch, base_sha, head_sha, pr_url, delivery_error, verify}'

jq -e '.status == "achieved"' <<<"${RESULT_JSON}" >/dev/null || fail "Expected status == achieved"
[[ -n "${PR_URL}" ]] || fail "Expected non-empty pr_url in result"
jq -e '(.verify | length == 1) and (.verify[0].exit_code == 0)' <<<"${RESULT_JSON}" >/dev/null || fail "Expected passing go test ./... verification"

printf '\n[PASS] job_id=%s status=achieved pr_url=%s last_seq=%s\n' "${JOB_ID}" "${PR_URL}" "${LAST_SEQ}"
