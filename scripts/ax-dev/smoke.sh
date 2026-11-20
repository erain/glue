#!/usr/bin/env bash
# scripts/ax-dev/smoke.sh — end-to-end smoke test for the Glue AX kind cluster.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

log() { printf '\n==> %s\n' "$*"; }
fail() { printf 'SMOKE FAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"; }

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  fail "kind cluster '${KIND_CLUSTER_NAME}' is not running (run scripts/ax-dev/up.sh)"
fi
[[ -f "${STATE}/images.env" ]] && { # shellcheck disable=SC1090,SC1091
  source "${STATE}/images.env"; }
TASK_IMAGE="${AX_DEFAULT_TASK_IMAGE:-${GLUE_TASK_IMAGE:-}}"
[[ -n "${TASK_IMAGE}" ]] || fail "missing AX_DEFAULT_TASK_IMAGE in ${STATE}/images.env (run scripts/ax-dev/up.sh)"

TMP_DIR="$(mktemp -d -t glue-ax-smoke-XXXXXX)"
export AX_HOME="${TMP_DIR}/ax-home"
TASK_NAME="smoke-$(date +%s)"
GW_NAME="${TASK_NAME}-gw"
PROXY_PORT="${GLUE_SMOKE_PROXY_PORT:-18080}"
PF_PID=""
PROXY_PID=""

cleanup() {
  local rc=$?
  set +e
  [[ -n "${PROXY_PID}" ]] && kill "${PROXY_PID}" >/dev/null 2>&1 && wait "${PROXY_PID}" 2>/dev/null
  if [[ -n "${AX_SERVER_URL:-}" ]]; then
    ax --server "${AX_SERVER_URL}" --context "${KUBE_CONTEXT}" -a default delete task "${TASK_NAME}" >/dev/null 2>&1
    ax --server "${AX_SERVER_URL}" --context "${KUBE_CONTEXT}" -a default delete gateway "${GW_NAME}" >/dev/null 2>&1
  fi
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" >/dev/null 2>&1 && wait "${PF_PID}" 2>/dev/null
  ax tunnel stop "${KUBE_CONTEXT}" >/dev/null 2>&1 || true
  rm -rf "${TMP_DIR}"
  exit "${rc}"
}
trap cleanup EXIT

k -n "${AX_SYSTEM_NAMESPACE}" port-forward svc/ax-server 0:8080 >"${TMP_DIR}/pf.log" 2>&1 &
PF_PID=$!
AX_PORT=""
for _ in $(seq 1 50); do
  AX_PORT="$(sed -nE 's/.*127\.0\.0\.1:([0-9]+).*/\1/p' "${TMP_DIR}/pf.log" | head -n1)"
  [[ -n "${AX_PORT}" ]] && curl -fsS "http://127.0.0.1:${AX_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
[[ -n "${AX_PORT}" ]] || fail "ax-server port-forward did not become ready"
AX_SERVER_URL="http://127.0.0.1:${AX_PORT}"
run_ax() { ax --server "${AX_SERVER_URL}" --context "${KUBE_CONTEXT}" -a default "$@"; }
task_state() { run_ax get task "${TASK_NAME}" -o yaml | awk '$1 == "state:" && !p++ {gsub(/"/, "", $2); print $2}'; }

write_task_yaml() {
  local gw_line=""
  [[ -n "${1:-}" ]] && gw_line="  gateway: {name: $1}"$'\n'
  cat > "${TMP_DIR}/task.yaml" <<EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${TASK_NAME}
  atespace: default
spec:
  debug: true
${gw_line}  profile:
    image: ${TASK_IMAGE}
  commands:
    - python3
    - -c
    - |
      import http.server, socketserver
      class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
          self.send_response(200); self.end_headers(); self.wfile.write(b"glue-ax-proxy-ok\n")
        def log_message(self, *args): pass
      socketserver.TCPServer(("0.0.0.0", 8080), H).serve_forever()
EOF
}

log "1. Applying task '${TASK_NAME}', resuming, verifying Working and ax ssh..."
write_task_yaml ""
run_ax apply -f "${TMP_DIR}/task.yaml"
run_ax resume task "${TASK_NAME}"
state="$(task_state)"
[[ "${state}" == "TASK_STATE_WORKING" ]] || fail "expected state TASK_STATE_WORKING, got ${state}"
ssh_out="$(run_ax ssh "${TASK_NAME}" -- echo ok | tr -d '\r')"
[[ "${ssh_out}" == "ok" ]] || fail "expected 'ax ssh -- echo ok' to print 'ok', got: ${ssh_out}"
printf '  [ok] task %s is Working and ax ssh printed: %s\n' "${TASK_NAME}" "${ssh_out}"

log "2. Verifying default-deny egress (getent hosts github.com must fail)..."
if run_ax ssh "${TASK_NAME}" -- getent hosts github.com >/dev/null 2>&1; then
  fail "expected 'getent hosts github.com' to fail without a Gateway"
fi
printf '  [ok] getent hosts github.com failed as expected without Gateway\n'

log "3. Binding AX Gateway '${GW_NAME}' (tls_passthrough github.com:443) and verifying git ls-remote..."
run_ax delete task "${TASK_NAME}"
cat > "${TMP_DIR}/gateway.yaml" <<EOF
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: ${GW_NAME}
  atespace: default
spec:
  egress:
    - tls_passthrough:
        hostnames: ["github.com"]
        ports:
          numbers: [443]
EOF
run_ax apply -f "${TMP_DIR}/gateway.yaml"
write_task_yaml "${GW_NAME}"
run_ax apply -f "${TMP_DIR}/task.yaml"
run_ax resume task "${TASK_NAME}"
ls_remote=""
for _ in $(seq 1 15); do
  if ls_remote="$(run_ax ssh "${TASK_NAME}" -- git ls-remote https://github.com/google/ax HEAD 2>/dev/null)" && [[ "${ls_remote}" == *"HEAD"* ]]; then break; fi
  sleep 1
done
[[ "${ls_remote}" == *"HEAD"* ]] || fail "git ls-remote https://github.com/google/ax HEAD did not succeed after Gateway bind"
printf '  [ok] git ls-remote succeeded: %s\n' "${ls_remote}"

log "4. Verifying arbitrary-port ingress via 'kubectl ate proxy actor ${TASK_NAME} ${PROXY_PORT}:8080'..."
kubectl ate --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" proxy actor "${TASK_NAME}" "${PROXY_PORT}:8080" -a default >"${TMP_DIR}/proxy.log" 2>&1 &
PROXY_PID=$!
proxy_out=""
for _ in $(seq 1 50); do
  if proxy_out="$(curl -fsS --max-time 2 "http://127.0.0.1:${PROXY_PORT}/" 2>/dev/null)" && [[ "${proxy_out}" == "glue-ax-proxy-ok" ]]; then break; fi
  sleep 0.2
done
kill "${PROXY_PID}" >/dev/null 2>&1 || true; wait "${PROXY_PID}" 2>/dev/null || true; PROXY_PID=""
[[ "${proxy_out}" == "glue-ax-proxy-ok" ]] || fail "proxy to port 8080 failed (got: ${proxy_out})"
printf '  [ok] kubectl ate proxy %s:8080 returned: %s\n' "${PROXY_PORT}" "${proxy_out}"

log "5. Verifying suspend/resume persistence (/workspace survives; /tmp is reset)..."
run_ax ssh "${TASK_NAME}" -- sh -c 'echo survive-416 > /workspace/keep.txt && echo vanish-416 > /tmp/drop.txt'
run_ax suspend task "${TASK_NAME}"
sub_state="$(task_state)"
[[ "${sub_state}" == "TASK_STATE_SUSPENDED" ]] || fail "expected state TASK_STATE_SUSPENDED, got ${sub_state}"
run_ax resume task "${TASK_NAME}"
persist_out="$(run_ax ssh "${TASK_NAME}" -- sh -c 'cat /workspace/keep.txt && if [ -e /tmp/drop.txt ]; then echo TMP_PRESENT; else echo TMP_GONE; fi' | tr -d '\r')"
[[ "${persist_out}" == $'survive-416\nTMP_GONE' ]] || fail "unexpected post-resume state: ${persist_out}"
printf '  [ok] /workspace/keep.txt survived (survive-416) and /tmp/drop.txt was wiped (TMP_GONE)\n'

log "6. Cleaning up task '${TASK_NAME}' and gateway '${GW_NAME}' (leaving '${KIND_CLUSTER_NAME}' running)..."
run_ax delete task "${TASK_NAME}"
run_ax delete gateway "${GW_NAME}"
if run_ax get task "${TASK_NAME}" >/dev/null 2>&1; then fail "task ${TASK_NAME} still exists after delete"; fi
printf '  [ok] task %s and gateway %s deleted\n\nSMOKE PASS: all 6 assertions succeeded.\n' "${TASK_NAME}" "${GW_NAME}"
