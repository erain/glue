#!/usr/bin/env bash
# scripts/ax-dev/manual-job.sh — run a Glue coding job end to end inside an AX Task on kind.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

usage() {
  cat <<'EOF'
Usage: scripts/ax-dev/manual-job.sh --repo <git-url> --objective <text> [options]

Options:
  --repo <url>            Target Git repository URL (required)
  --objective <text>      Coding objective for the Glue job (required)
  --base <branch>         Base branch to fetch and target (default: main)
  --name <task-name>      AX Task name (default: glue-manual; Workspace is <name>-ws)
  --policy <profile>      Permission policy: workspace-auto, ask-all, or yolo (default: workspace-auto)
  --deliver <mode>        Delivery mode: draft_pr, branch, or none (default: draft_pr)
  --verify <cmd>          Verification command (repeatable; default: "go test ./...")
  --no-verify             Run no post-goal verification commands
  --max-iterations <n>    Maximum goal loop iterations (default: 10)
  --provider <name>       Model provider override
  --model <id>            Model ID override
  --cpu <limit>           Task CPU limit (default: 4)
  --memory <limit>        Task memory limit (default: 8Gi)
  --port <local-port>     Local port for kubectl ate proxy (default: 18080)
  --recreate              Delete any existing Task/Workspace with --name before creating
  --no-start              Stop after verifying golden boot (status=created) without starting
  --no-attach             Start the job and print connection info without attaching
  --keep-proxy            Leave ax-server port-forward and kubectl ate proxy running on exit
EOF
}

REPO_URL="" OBJECTIVE="" BASE_REF="main" TASK_NAME="glue-manual"
POLICY="workspace-auto" DELIVERY="draft_pr" VERIFY_CMDS=() NO_VERIFY="false"
MAX_ITERATIONS=10 PROVIDER="" MODEL="" CPU_LIMIT="4" MEMORY_LIMIT="8Gi"
PROXY_PORT="${GLUE_AX_PROXY_PORT:-18080}"
RECREATE="false" NO_START="false" NO_ATTACH="false" KEEP_PROXY="false"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) REPO_URL="$2"; shift 2 ;;
    --objective) OBJECTIVE="$2"; shift 2 ;;
    --base) BASE_REF="$2"; shift 2 ;;
    --name) TASK_NAME="$2"; shift 2 ;;
    --policy) POLICY="$2"; shift 2 ;;
    --deliver|--delivery) DELIVERY="$2"; shift 2 ;;
    --verify) VERIFY_CMDS+=("$2"); shift 2 ;;
    --no-verify) NO_VERIFY="true"; shift ;;
    --max-iterations) MAX_ITERATIONS="$2"; shift 2 ;;
    --provider) PROVIDER="$2"; shift 2 ;;
    --model) MODEL="$2"; shift 2 ;;
    --cpu) CPU_LIMIT="$2"; shift 2 ;;
    --memory) MEMORY_LIMIT="$2"; shift 2 ;;
    --port) PROXY_PORT="$2"; shift 2 ;;
    --recreate) RECREATE="true"; shift ;;
    --no-start) NO_START="true"; shift ;;
    --no-attach) NO_ATTACH="true"; shift ;;
    --keep-proxy) KEEP_PROXY="true"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "${REPO_URL}" && -n "${OBJECTIVE}" ]] || { usage >&2; exit 2; }
[[ "${NO_VERIFY}" == "false" && ${#VERIFY_CMDS[@]} -eq 0 ]] && VERIFY_CMDS=("go test ./...")

log() { printf '\n==> %s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"; }
now_ms() { date +%s%3N; }

[[ -f "${STATE}/images.env" ]] || fail "missing ${STATE}/images.env; run scripts/ax-dev/up.sh && scripts/ax-dev/build-task-image.sh first"
# shellcheck disable=SC1090,SC1091
source "${STATE}/images.env"
[[ -n "${GLUE_TASK_IMAGE:-}" ]] || fail "GLUE_TASK_IMAGE not set in ${STATE}/images.env; run scripts/ax-dev/build-task-image.sh first"

GITHUB_TOKEN="${GLUE_GITHUB_TOKEN:-}"
if [[ -z "${GITHUB_TOKEN}" ]] && command -v gh >/dev/null 2>&1; then
  GITHUB_TOKEN="$(gh auth token 2>/dev/null || true)"
fi
[[ -n "${GITHUB_TOKEN}" ]] || fail "GLUE_GITHUB_TOKEN is unset and 'gh auth token' returned empty"
[[ -n "${GEMINI_API_KEY:-}" || -n "${OPENROUTER_API_KEY:-}" ]] || fail "GEMINI_API_KEY (or OPENROUTER_API_KEY) must be set in the environment"

mkdir -p "${STATE}/bin"
log "Building host glue CLI in ${STATE}/bin/glue..."
(cd "${REPO_ROOT}" && go build -o "${STATE}/bin/glue" ./cmd/glue)
export PATH="${STATE}/bin:${PATH}"

TMP_DIR="$(mktemp -d -t glue-ax-job-XXXXXX)"
chmod 0700 "${TMP_DIR}"
export AX_HOME="${TMP_DIR}/ax-home"
PF_PID="" PROXY_PID=""

cleanup() {
  local rc=$?
  set +e
  if [[ "${KEEP_PROXY}" != "true" ]]; then
    [[ -n "${PROXY_PID}" ]] && kill "${PROXY_PID}" >/dev/null 2>&1 && wait "${PROXY_PID}" 2>/dev/null
    [[ -n "${PF_PID}" ]] && kill "${PF_PID}" >/dev/null 2>&1 && wait "${PF_PID}" 2>/dev/null
  else
    printf '\n[keep-proxy] ax-server (%s, pid=%s) and proxy (http://127.0.0.1:%s, pid=%s) left running.\n' \
      "${AX_SERVER_URL:-}" "${PF_PID}" "${PROXY_PORT}" "${PROXY_PID}" >&2
  fi
  rm -rf "${TMP_DIR}"
  exit "${rc}"
}
trap cleanup EXIT

k -n "${AX_SYSTEM_NAMESPACE}" port-forward svc/ax-server 0:8080 >"${STATE}/ax-server-pf.log" 2>&1 </dev/null &
PF_PID=$!
AX_PORT=""
for _ in $(seq 1 50); do
  AX_PORT="$(sed -nE 's/.*127\.0\.0\.1:([0-9]+).*/\1/p' "${STATE}/ax-server-pf.log" | head -n1)"
  [[ -n "${AX_PORT}" ]] && curl -fsS "http://127.0.0.1:${AX_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
[[ -n "${AX_PORT}" ]] || fail "ax-server port-forward did not become ready"
export AX_SERVER_URL="http://127.0.0.1:${AX_PORT}"
run_ax() { ax --server "${AX_SERVER_URL}" --context "${KUBE_CONTEXT}" -a default "$@"; }

WS_NAME="${TASK_NAME}-ws"
if run_ax get task "${TASK_NAME}" >/dev/null 2>&1; then
  if [[ "${RECREATE}" == "true" ]]; then
    log "Deleting existing task '${TASK_NAME}' and workspace '${WS_NAME}' (--recreate)..."
    run_ax delete task "${TASK_NAME}" >/dev/null 2>&1 || true
    run_ax delete workspace "${WS_NAME}" >/dev/null 2>&1 || true
    kubectl ate --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" delete egress-policy "${TASK_NAME}" -a default >/dev/null 2>&1 || true
  else
    log "Task '${TASK_NAME}' already exists; reusing existing task (pass --recreate to start fresh)."
  fi
fi

if ! run_ax get task "${TASK_NAME}" >/dev/null 2>&1; then
  log "1. Creating AX Workspace '${WS_NAME}' (${REPO_URL} @ ${BASE_REF}, depth=1, dir=.)..."
  jq -n --arg ws "${WS_NAME}" --arg repo "${REPO_URL}" --arg branch "${BASE_REF}" '{
    apiVersion: "ax.io/v1alpha1", kind: "Workspace", metadata: {name: $ws, atespace: "default"},
    spec: {git: [{name: "repo", repo: $repo, branch: $branch, dir: ".", depth: 1}]}
  }' > "${TMP_DIR}/workspace.yaml"
  run_ax apply -f "${TMP_DIR}/workspace.yaml"

  log "2. Creating AX Task '${TASK_NAME}' (image=${GLUE_TASK_IMAGE}, limits cpu=${CPU_LIMIT} memory=${MEMORY_LIMIT})..."
  t0="$(now_ms)"
  VERIFY_JSON="[]"
  [[ ${#VERIFY_CMDS[@]} -gt 0 ]] && VERIFY_JSON="$(printf '%s\n' "${VERIFY_CMDS[@]}" | jq -R . | jq -cs .)"
  CONFIG_JSON="$(jq -cn \
    --arg objective "${OBJECTIVE}" --arg base_ref "${BASE_REF}" --arg policy "${POLICY}" \
    --arg delivery "${DELIVERY}" --arg provider "${PROVIDER}" --arg model "${MODEL}" \
    --argjson max_iterations "${MAX_ITERATIONS}" --argjson verify "${VERIFY_JSON}" '{
      objective: $objective, repo: {path: "/workspace", base_ref: $base_ref},
      policy: $policy, delivery: $delivery, budgets: {max_iterations: $max_iterations}, verify: $verify
    } + (if $provider != "" then {provider: $provider} else {} end) + (if $model != "" then {model: $model} else {} end)')"
  ENV_JSON="$(jq -cn --arg gh "${GITHUB_TOKEN}" --arg gemini "${GEMINI_API_KEY:-}" --arg openrouter "${OPENROUTER_API_KEY:-}" \
    '[{name: "GLUE_GITHUB_TOKEN", value: $gh}]
    + (if $gemini != "" then [{name: "GEMINI_API_KEY", value: $gemini}] else [] end)
    + (if $openrouter != "" then [{name: "OPENROUTER_API_KEY", value: $openrouter}] else [] end)')"

  jq -n \
    --arg name "${TASK_NAME}" --arg ws "${WS_NAME}" --arg image "${GLUE_TASK_IMAGE}" \
    --arg cpu "${CPU_LIMIT}" --arg memory "${MEMORY_LIMIT}" \
    --argjson env "${ENV_JSON}" --argjson config "${CONFIG_JSON}" '{
      apiVersion: "ax.io/v1alpha1", kind: "Task", metadata: {name: $name, atespace: "default"},
      spec: {
        debug: true, image: $image, workspace: {name: $ws, path: "/workspace"},
        resources: {limits: {cpu: $cpu, memory: $memory}},
        command: ["glue", "serve", "--coding", "--jobs", "--job-from-ax", "--listen", ":8080", "--state-dir", "/workspace/.glue/state", "--token-file", "/workspace/.glue/token"],
        env: $env, config: $config
      }
    }' > "${TMP_DIR}/task.yaml"
  chmod 0600 "${TMP_DIR}/task.yaml"
  run_ax apply -f "${TMP_DIR}/task.yaml"
  rm -f "${TMP_DIR}/task.yaml"
  printf '  [ok] Task %s created in Suspended phase (%d ms)\n' "${TASK_NAME}" "$(( $(now_ms) - t0 ))"
fi

log "3. Applying EgressPolicy (tls_passthrough:443) for '${TASK_NAME}' before resume..."
kubectl ate --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" delete egress-policy "${TASK_NAME}" -a default >/dev/null 2>&1 || true
cat <<'EOF' | kubectl ate --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" create egress-policy "${TASK_NAME}" -a default -f -
rules:
- tls_passthrough:
    hostnames: [github.com, api.github.com, codeload.github.com, proxy.golang.org, sum.golang.org, storage.googleapis.com, generativelanguage.googleapis.com, openrouter.ai]
    ports: {numbers: [443]}
EOF

log "4. Resuming task '${TASK_NAME}' and waiting for Running + WorkspaceReady..."
t_resume="$(now_ms)"
run_ax resume task "${TASK_NAME}"
for _ in $(seq 1 60); do
  task_yaml="$(run_ax get task "${TASK_NAME}" -o yaml)"
  phase="$(awk '$1 == "phase:" && !p++ {print $2}' <<<"${task_yaml}" | tr -d '"')"
  ws_ready="$(awk '/type:[[:space:]]*WorkspaceReady/ {f=1; next} f && $1 == "status:" {gsub(/"/, "", $2); print $2; exit}' <<<"${task_yaml}")"
  [[ "${phase}" == "Running" && "${ws_ready}" == "True" ]] && break
  sleep 0.5
done
[[ "${phase:-}" == "Running" && "${ws_ready:-}" == "True" ]] || fail "Task ${TASK_NAME} did not reach Running + WorkspaceReady (phase=${phase:-}, WorkspaceReady=${ws_ready:-})"
printf '  [ok] Task %s is Running and WorkspaceReady=True (%d ms)\n' "${TASK_NAME}" "$(( $(now_ms) - t_resume ))"

log "5. Starting 'kubectl ate proxy actor ${TASK_NAME} ${PROXY_PORT}:8080 -a default' and reading bearer token..."
pkill -f "kubectl[- ]ate.*proxy actor .* ${PROXY_PORT}:8080" >/dev/null 2>&1 || true
kubectl ate --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" proxy actor "${TASK_NAME}" "${PROXY_PORT}:8080" -a default >"${STATE}/ate-proxy.log" 2>&1 </dev/null &
PROXY_PID=$!
SERVER_URL="http://127.0.0.1:${PROXY_PORT}"

DAEMON_TOKEN=""
for _ in $(seq 1 30); do
  DAEMON_TOKEN="$(run_ax ssh "${TASK_NAME}" -- sh -c 'cat /workspace/.glue/token 2>/dev/null' | tr -d '\r\n' || true)"
  [[ -n "${DAEMON_TOKEN}" ]] && break
  sleep 0.5
done
[[ -n "${DAEMON_TOKEN}" ]] || fail "Timed out waiting for /workspace/.glue/token inside ${TASK_NAME}"
glue job login --server "${SERVER_URL}" --token "${DAEMON_TOKEN}" >/dev/null

JOBS_JSON=""
for _ in $(seq 1 50); do
  if JOBS_JSON="$(glue job list --server "${SERVER_URL}" --json 2>/dev/null)" && [[ -n "${JOBS_JSON}" ]]; then break; fi
  sleep 0.2
done
[[ -n "${JOBS_JSON}" ]] || fail "Proxy ${SERVER_URL} did not respond to 'glue job list'"

JOB_ID="$(jq -r '.jobs[0].id // empty' <<<"${JOBS_JSON}")"
JOB_STATUS="$(jq -r '.jobs[0].status // empty' <<<"${JOBS_JSON}")"
[[ -n "${JOB_ID}" ]] || fail "No job found on ${SERVER_URL} (was --job-from-ax enabled?)"
printf '  [ok] Connected to %s: job_id=%s status=%s\n' "${SERVER_URL}" "${JOB_ID}" "${JOB_STATUS}"

log "6. Verifying golden boot, starting job ${JOB_ID}, and attaching..."
if [[ "${JOB_STATUS}" == "created" ]]; then
  printf '  [ok] Golden boot verified: job %s is in status "created" before explicit start.\n' "${JOB_ID}"
  if [[ "${NO_START}" == "true" ]]; then
    printf '  [--no-start] Leaving job %s in status "created" (task=%s, server=%s).\n' "${JOB_ID}" "${TASK_NAME}" "${SERVER_URL}"
    exit 0
  fi
  glue job start --server "${SERVER_URL}" "${JOB_ID}"
else
  printf '  [info] Job %s already in status "%s"; skipping start.\n' "${JOB_ID}" "${JOB_STATUS}"
fi

if [[ "${NO_ATTACH}" == "true" ]]; then
  printf '  [--no-attach] Job %s running on %s (task=%s).\n' "${JOB_ID}" "${SERVER_URL}" "${TASK_NAME}"
  exit 0
fi

glue job attach --server "${SERVER_URL}" "${JOB_ID}"
log "Job ${JOB_ID} result summary:"
glue job result --server "${SERVER_URL}" "${JOB_ID}" --json | jq '{job_id, status, branch, base_sha, head_sha, pr_url, delivery_error, verify}'
