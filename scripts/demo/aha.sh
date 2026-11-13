#!/usr/bin/env bash
# Copyright (c) 2026 Yu YI
# SPDX-License-Identifier: MIT
#
# aha.sh runs the remote-mode quickstart story end to end on the standing
# kind-glue-ax cluster against erain/glue-sandbox, and fails on any miss:
#   doctor -> uncommitted work sent with --snapshot -> two attempts ->
#   phone notification (ntfy.sh topic) -> compare -> pull --apply -> local tests.
#
# Usage: scripts/demo/aha.sh   (GLUE_BIN=path/to/glue to test a build)
# Needs: GEMINI_API_KEY (or OPENROUTER_API_KEY), a GitHub token (gh auth login),
# go, git, curl, python3, and `source scripts/ax-dev/env.sh` done by the caller
# or below.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source=/dev/null
[[ -n "${GLUE_AX_CONTEXT:-}" ]] || source "$ROOT/scripts/ax-dev/env.sh"
# Prove what a new user gets: the published task image. ax-dev's env.sh
# exports a locally built GLUE_TASK_IMAGE, which may be stale.
unset GLUE_TASK_IMAGE
G=${GLUE_BIN:-glue}
REPO=${AHA_REPO:-https://github.com/erain/glue-sandbox}
SUBMIT_BUDGET_S=${SUBMIT_BUDGET_S:-5}   # per job, submit -> job ID
JOB_BUDGET_S=${JOB_BUDGET_S:-240}       # submit -> both attempts finished

fail() { echo "AHA FAIL: $*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
now() { date +%s.%N; }
secs() { python3 -c "print(f'{$2-$1:.1f}')"; }

WORK=$(mktemp -d)
IDS=()
# ...and ax-dev's images.env under the cache directory names one too. An empty
# cache directory gives the published default, pinned by digest.
export GOCACHE=${GOCACHE:-$(go env GOCACHE)} GOMODCACHE=${GOMODCACHE:-$(go env GOMODCACHE)}
export XDG_CACHE_HOME="$WORK/cache"
cleanup() {
  for id in "${IDS[@]}"; do
    "$G" job delete "$id" >/dev/null 2>&1 || true
    git -C "$WORK/repo" push -q origin --delete "glue/$id" >/dev/null 2>&1 || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

step "A checkout with uncommitted work"
git clone -q "$REPO" "$WORK/repo"
cd "$WORK/repo"
cat > go.mod <<'GO'
module example.com/sandbox

go 1.24
GO
cat > add.go <<'GO'
package sandbox

// Add returns a + b.
func Add(a, b int) int { return a + b }
GO
git status --short

step "glue job doctor"
"$G" job doctor || fail "doctor reported a problem"

step "Two attempts of one task, with your uncommitted work, notifying your phone"
TOPIC="glue-aha-$(python3 -c 'import secrets; print(secrets.token_hex(6))')"
NOTIFY="https://ntfy.sh/$TOPIC"
t0=$(now)
"$G" --remote "Add a function Sub(a, b int) int next to Add in add.go, with a table-driven test in add_test.go that covers Add and Sub." \
  --snapshot --attempts 2 --verify "go test ./..." --notify "$NOTIFY" --quiet >"$WORK/ids" 2>"$WORK/submit.err" || { cat "$WORK/submit.err"; fail "submit"; }
t1=$(now)
cat "$WORK/submit.err"
mapfile -t IDS <"$WORK/ids"
GROUP=$(grep -o 'grp-[0-9a-f]*' "$WORK/submit.err" | head -1)
[[ ${#IDS[@]} -eq 2 && -n "$GROUP" ]] || fail "expected two job IDs and a group, got: ${IDS[*]} / $GROUP"
per_job=$(python3 -c "print(f'{($t1-$t0)/2:.1f}')")
echo "submitted ${IDS[*]} in $(secs "$t0" "$t1")s (${per_job}s per job, budget ${SUBMIT_BUDGET_S}s)"
python3 -c "import sys; sys.exit(0 if $per_job <= $SUBMIT_BUDGET_S else 1)" || fail "submit took ${per_job}s per job"

step "Watch until both finish"
for id in "${IDS[@]}"; do timeout "$JOB_BUDGET_S" "$G" job attach "$id" | tail -12 || true; done
t2=$(now)
echo "both finished $(secs "$t0" "$t2")s after submit (budget ${JOB_BUDGET_S}s)"

step "glue job compare $GROUP"
"$G" job compare "$GROUP" | tee "$WORK/compare"
[[ $(grep -c 'achieved  *pass' "$WORK/compare") -ge 1 ]] || fail "no attempt achieved with verify passing"

step "Phone notifications (ntfy.sh/$TOPIC)"
got=0
for _ in $(seq 1 15); do
  got=$(curl -fsS "https://ntfy.sh/$TOPIC/json?poll=1&since=15m" | grep -c '"event":"message"' || true)
  [[ $got -ge 2 ]] && break
  sleep 2
done
echo "received $got notification(s)"
[[ $got -ge 2 ]] || fail "expected a done notification per attempt"

step "Bring the best attempt home"
BEST=$(awk '/achieved +pass/ {print $1; exit}' "$WORK/compare")
"$G" job pull "$BEST" --apply
git status --short
go test ./... || fail "local tests failed after pull"

total=$(secs "$t0" "$(now)")
printf '\nAHA OK: %s attempts, best %s, %ss from submit to tested code in your checkout\n' "${#IDS[@]}" "$BEST" "$total"
