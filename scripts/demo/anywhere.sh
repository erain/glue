#!/usr/bin/env bash
# Copyright (c) 2026 Yu YI
# SPDX-License-Identifier: MIT
#
# anywhere.sh runs the Phase 4 remote-mode experience end to end on kind-glue-ax
# + headless Chrome against erain/glue-sandbox:
#   1. glue remote setup (Cloudflare Access, 0600 config.yaml, 0 idle AX workers)
#   2. 390x844 mobile browser: zero-prompt login -> interactive 2-turn chat ->
#      1-click Run Autonomously (verify: go test ./...) -> unified diff -> Draft PR
#   3. closed-laptop ntfy lock-screen HTTP action tap-back (zero --notify flag)
#   4. glue job teleport <id> --no-tui -> local go test ./... + session briefing
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source=/dev/null
[[ -n "${GLUE_AX_CONTEXT:-}" ]] || source "$ROOT/scripts/ax-dev/env.sh"
REPO=${ANYWHERE_REPO:-https://github.com/erain/glue-sandbox}
SETUP_BUDGET_S=${SETUP_BUDGET_S:-120}
BROWSER_BUDGET_S=${BROWSER_BUDGET_S:-300}
NTFY_BUDGET_S=${NTFY_BUDGET_S:-240}
TELEPORT_BUDGET_S=${TELEPORT_BUDGET_S:-30}

fail() { echo "ANYWHERE FAIL: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
now() { date +%s.%N; }
secs() { python3 -c "print(f'{$2-$1:.1f}')"; }
within() { python3 -c "import sys; sys.exit(0 if float('$1') <= float('$2') else 1)" || fail "$3 took ${1}s (budget ${2}s)"; }

[[ -n "${GEMINI_API_KEY:-}${OPENROUTER_API_KEY:-}" ]] || fail "set GEMINI_API_KEY or OPENROUTER_API_KEY"
gh auth token >/dev/null 2>&1 || [[ -n "${GITHUB_TOKEN:-}${GLUE_GITHUB_TOKEN:-}" ]] || fail "run gh auth login or set GITHUB_TOKEN"

WORK=$(mktemp -d)
IDS=()
EDGE_PID=""
PF_PID=""
export GOCACHE=${GOCACHE:-$(go env GOCACHE)} GOMODCACHE=${GOMODCACHE:-$(go env GOMODCACHE)}
mkdir -p "$WORK/bin"
if [[ -n "${GLUE_BIN:-}" ]]; then G="$GLUE_BIN"; else go build -o "$WORK/bin/glue" "$ROOT/cmd/glue"; G="$WORK/bin/glue"; fi
go build -o "$WORK/bin/anywhere_driver" "$ROOT/scripts/demo/anywhere_driver.go"

cleanup() {
  # shellcheck source=/dev/null
  [[ -f "$WORK/browser.env" ]] && source "$WORK/browser.env" || true
  [[ -n "${BROWSER_JOB_ID:-}" ]] && IDS+=("$BROWSER_JOB_ID")
  [[ -n "${PR_URL:-}" ]] && { gh pr close "$PR_URL" --delete-branch >/dev/null 2>&1 || gh pr close "$PR_URL" >/dev/null 2>&1 || true; }
  local slug=${REPO#https://github.com/}
  slug=${slug%.git}
  for id in "${IDS[@]}"; do
    [[ -n "$id" ]] || continue
    "$G" job delete "$id" --ax >/dev/null 2>&1 || true
    gh api -X DELETE "repos/$slug/git/refs/heads/glue/$id" >/dev/null 2>&1 || true
  done
  [[ -n "$PF_PID" ]] && kill "$PF_PID" >/dev/null 2>&1 || true
  [[ -n "$EDGE_PID" ]] && kill "$EDGE_PID" >/dev/null 2>&1 || true
  "$G" remote down --purge-pvc >/dev/null 2>&1 || true
  kubectl wait --for=delete ns/glue-gateway --timeout=60s >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

t_all=$(now)
if [[ "${ANYWHERE_BUILD_LOCAL:-0}" == "1" ]] || ! docker image inspect "${GLUE_TASK_IMAGE:-}" >/dev/null 2>&1; then
  TASK_IMAGE=$("$ROOT/scripts/ax-dev/build-task-image.sh")
else
  TASK_IMAGE="$GLUE_TASK_IMAGE"
fi
export GLUE_TASK_IMAGE="$TASK_IMAGE"

step "1. One-time cluster setup + Cloudflare Access (0 idle AX workers)"
export GLUE_CONFIG_DIR="$WORK/config/glue"
mkdir -p -m 0700 "$GLUE_CONFIG_DIR"
TOPIC="glue-anywhere-$(python3 -c 'import secrets; print(secrets.token_hex(8))')"
PROV_CFG="provider: openrouter"
[[ -n "${GEMINI_API_KEY:-}" ]] && PROV_CFG=$'provider: gemini\nmodel: gemini-3-flash-preview'
printf 'notify:\n  - https://ntfy.sh/%s\n%s\n' "$TOPIC" "$PROV_CFG" >"$GLUE_CONFIG_DIR/config.yaml"
chmod 0600 "$GLUE_CONFIG_DIR/config.yaml"

BRIDGE_IP=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -E '^[0-9]+\.' | head -1)
GW_PF_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
"$WORK/bin/anywhere_driver" edge "$WORK/edge.env" "$GW_PF_PORT" &
EDGE_PID=$!
for _ in $(seq 1 50); do [[ -f "$WORK/edge.env" ]] && break; sleep 0.1; done
# shellcheck source=/dev/null
source "$WORK/edge.env"

t0=$(now)
"$G" remote setup --build-local --image "$TASK_IMAGE" --public-url https://glue.anywhere.example \
  --cf-team anywhere.cloudflareaccess.com --cf-aud aud-anywhere --allow-email dev@anywhere.example \
  --cf-certs-url "http://${BRIDGE_IP}:${JWKS_PORT}/certs"
"$G" remote status | tee "$WORK/status.txt"
t_setup=$(secs "$t0" "$(now)")
within "$t_setup" "$SETUP_BUDGET_S" "glue remote setup"
grep -q 'ready: 1/1.*auth: cloudflare-access.*public_url: https://glue.anywhere.example.*healthz: ok.*backends: ok (ax, access-protected)' "$WORK/status.txt" || fail "unexpected status output"
[[ "$(stat -c '%a' "$GLUE_CONFIG_DIR/config.yaml")" == "600" ]] && grep -q 'public_url: https://glue.anywhere.example' "$GLUE_CONFIG_DIR/config.yaml" || fail "config.yaml 0600 public_url check failed"
idle_running=$("$G" job list --ax --status running --json | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("jobs") or []))')
[[ "$idle_running" == "0" ]] || fail "expected 0 running AX tasks while idle, got $idle_running"

kubectl port-forward -n glue-gateway svc/glue-gateway "${GW_PF_PORT}:8800" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 50); do curl -fsS "http://127.0.0.1:${GW_PF_PORT}/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
[[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${GW_PF_PORT}/v1/jobs")" == "401" ]] || fail "unauthenticated /v1/jobs did not return 401"
curl -ksS "${EDGE_URL}/v1/me" | grep -q '"login":"dev@anywhere.example"' || fail "edge /v1/me identity check failed"
echo "gateway ready in ${t_setup}s (0 running AX tasks, unauthenticated=401, CF Access=dev@anywhere.example)"

step "2. Mobile browser (390x844): interactive chat -> autonomous goal -> diff & Draft PR"
t0=$(now)
"$WORK/bin/anywhere_driver" browser "$EDGE_URL" "$REPO" "$WORK/browser.env" "$BROWSER_BUDGET_S"
t_browser=$(secs "$t0" "$(now)")
within "$t_browser" "$BROWSER_BUDGET_S" "browser interactive->goal->PR"
# shellcheck source=/dev/null
source "$WORK/browser.env"
pr_state=$(gh pr view "$PR_URL" --json state,isDraft --jq '"\(.state) draft=\(.isDraft)"')
[[ "$pr_state" == "OPEN draft=true" ]] || fail "expected OPEN draft=true for $PR_URL, got $pr_state"
echo "browser flow ok in ${t_browser}s: $BROWSER_JOB_ID -> $PR_URL ($pr_state)"

step "3. Closed-laptop ntfy tap-back (ntfy.sh/glue-anywhere-…, zero --notify flag)"
t0=$(now)
NTFY_JOB_ID=$("$G" --remote "Call ask_user once right now with question 'Which label should go in choice.txt?' and options ['Alpha', 'Beta'], then create choice.txt containing only the chosen word followed by a newline." \
  --ax --repo "$REPO" --image "$TASK_IMAGE" --verify "grep -qx Beta choice.txt" --max-iterations 3 --quiet)
IDS+=("$NTFY_JOB_ID")
"$WORK/bin/anywhere_driver" ntfy "https://ntfy.sh/$TOPIC" "$NTFY_JOB_ID" "https://glue.anywhere.example/#/jobs/$NTFY_JOB_ID" "$NTFY_BUDGET_S"
t_ntfy=$(secs "$t0" "$(now)")
within "$t_ntfy" "$NTFY_BUDGET_S" "ntfy tap-back"
"$G" job result "$NTFY_JOB_ID" --ax | grep -q '^status: achieved$' || fail "ntfy job $NTFY_JOB_ID did not achieve"
logs_json=$("$G" job logs "$NTFY_JOB_ID" --ax --json)
[[ "$logs_json" == *'"source":"ntfy"'* && "$logs_json" != *'glue-reply-'* ]] || fail "ntfy logs provenance/redaction check failed"
echo "ntfy tap-back ok in ${t_ntfy}s: $NTFY_JOB_ID achieved via lock-screen HTTP action"

step "4. Local code + context teleport (glue job teleport $BROWSER_JOB_ID --no-tui)"
t0=$(now)
git clone -q "$REPO" "$WORK/repo"
(cd "$WORK/repo" && "$G" job teleport "$BROWSER_JOB_ID" --ax --no-tui && go test ./...)
t_teleport=$(secs "$t0" "$(now)")
within "$t_teleport" "$TELEPORT_BUDGET_S" "glue job teleport + go test"
SESSION_FILE="$WORK/repo/.glue/sessions/teleport-${BROWSER_JOB_ID#job-}.json"
for needle in "# Remote job handoff: $BROWSER_JOB_ID" "## Objective" "## Plan checklist" "go test ./... (exit 0)"; do
  grep -Fq "$needle" "$SESSION_FILE" || fail "teleport briefing missing: $needle"
done
echo "teleport ok in ${t_teleport}s: verified $(basename "$SESSION_FILE") and local go test ./..."

t_total=$(secs "$t_all" "$(now)")
printf '\nANYWHERE OK: gateway %ss, browser %s -> %s (%ss), ntfy %s (%ss), teleport %ss (total %ss)\n' \
  "$t_setup" "$BROWSER_JOB_ID" "$PR_URL" "$t_browser" "$NTFY_JOB_ID" "$t_ntfy" "$t_teleport" "$t_total"
