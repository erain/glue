#!/usr/bin/env bash
# Copyright (c) 2026 Yu YI
# SPDX-License-Identifier: MIT
# scripts/ax-dev/e2e/latency.sh — run one small remote job on the standing AX
# cluster and report where the time goes: submit stages, time to first event,
# and model-turn latency. Exits non-zero when a latency budget is missed.
#
# Usage: latency.sh [--model <model>] [--thinking <level>] [--repo <url>]
# Budgets (override with env): SUBMIT_BUDGET_S=5 FIRST_EVENT_BUDGET_S=15 TURN_MEDIAN_BUDGET_S=5
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
REPO_URL="https://github.com/erain/glue-sandbox"
MODEL="" THINKING=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) REPO_URL="$2"; shift 2 ;;
    --model) MODEL="$2"; shift 2 ;;
    --thinking) THINKING="$2"; shift 2 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
SUBMIT_BUDGET_S="${SUBMIT_BUDGET_S:-5}"
FIRST_EVENT_BUDGET_S="${FIRST_EVENT_BUDGET_S:-15}"
TURN_MEDIAN_BUDGET_S="${TURN_MEDIAN_BUDGET_S:-5}"

# shellcheck source=scripts/ax-dev/env.sh disable=SC1091
source "${SCRIPT_DIR}/../env.sh"
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
TMP="$(mktemp -d -t glue-latency-XXXXXX)"
trap 'rm -rf "${TMP}"' EXIT
(cd "${REPO_ROOT}" && go build -o "${TMP}/glue" ./cmd/glue)
G="${TMP}/glue"

args=(--remote "Create a file hello.txt containing the single line hi." --repo "${REPO_URL}" --deliver none --max-iterations 1)
[[ -n "${MODEL}" ]] && args+=(--model "${MODEL}")
[[ -n "${THINKING}" ]] && args+=(--thinking "${THINKING}")

t0=$(date +%s.%N)
id=$("${G}" "${args[@]}" 2>"${TMP}/submit.err")
t_id=$(date +%s.%N)
trap '"${G}" job delete "${id}" >/dev/null 2>&1 || true; rm -rf "${TMP}"' EXIT
timeout 1200 "${G}" job attach "${id}" >/dev/null
"${G}" job logs "${id}" --json >"${TMP}/events.jsonl"

submit_s=$(echo "${t_id} - ${t0}" | bc)
# First event the job itself recorded after start (job_started), relative to submit.
first_ts=$(jq -r 'select(.type=="job_started") | .time' "${TMP}/events.jsonl" | head -1)
first_s=$(python3 -c "import sys,datetime as d; t=d.datetime.fromisoformat(sys.argv[1].replace('Z','+00:00')).timestamp(); print(round(t-float(sys.argv[2]),2))" "${first_ts}" "${t0}")
jq -r 'select(.type=="turn_end") | .payload.metadata.model_ms // empty' "${TMP}/events.jsonl" | sort -n >"${TMP}/turns"
turns=$(wc -l <"${TMP}/turns")
pct() { awk -v p="$1" '{a[NR]=$1} END {if (NR==0) {print 0; exit} i=int((NR-1)*p)+1; print a[i]/1000}' "${TMP}/turns"; }
median_s=$(pct 0.5) p90_s=$(pct 0.9)
total_s=$(jq -rs '(.[-1].time | sub("\\.[0-9]+";"") | fromdate) - (.[0].time | sub("\\.[0-9]+";"") | fromdate)' "${TMP}/events.jsonl")

printf '\nRemote job latency (%s)\n' "${id}"
printf '  %-28s %s\n' "submit stages:" ""
sed -n 's/^  \([a-z ]*[a-z]\) *\([0-9.]*s\)$/    \1 \2/p' "${TMP}/submit.err"
fail=0
check() { # name value budget
  local ok; ok=$(echo "$2 <= $3" | bc)
  printf '  %-28s %8.2fs   budget %ss   %s\n' "$1" "$2" "$3" "$([[ ${ok} == 1 ]] && echo ok || echo OVER)"
  [[ ${ok} == 1 ]] || fail=1
}
check "submit -> job ID:" "${submit_s}" "${SUBMIT_BUDGET_S}"
check "submit -> first job event:" "${first_s}" "${FIRST_EVENT_BUDGET_S}"
check "median model turn:" "${median_s}" "${TURN_MEDIAN_BUDGET_S}"
printf '  %-28s %8.2fs   (%d turns)\n' "p90 model turn:" "${p90_s}" "${turns}"
printf '  %-28s %8ss\n' "job run time (events):" "${total_s}"
exit "${fail}"
