#!/usr/bin/env bash
# Copyright (c) 2026 Yu YI
# SPDX-License-Identifier: MIT
# Offline command fixtures for the tracker audit; no GitHub access required.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
fixture_dir="$(mktemp -d)"
trap 'rm -rf -- "$fixture_dir"' EXIT
cat > "$fixture_dir/gh" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'issue list')
    printf '1\n2\n'
    if [[ "$TRACKER_FIXTURE" == broken ]]; then printf '3\n4\n'; fi
    ;;
  'repo view') printf '%s\n' 'fixture/glue' ;;
  'api graphql')
    if [[ "$TRACKER_FIXTURE" == unavailable ]]; then exit 1; fi
    if [[ "$TRACKER_FIXTURE" == missing ]]; then echo null; exit 0; fi
    number=''
    for arg in "$@"; do
      case "$arg" in number=*) number="${arg#number=}" ;; esac
    done
    case "$number" in
      1) printf '%s\n' '{"number":1,"title":"shipped","stateReason":"COMPLETED","closedByPullRequestsReferences":{"nodes":[{"state":"MERGED"}]}}' ;;
      2) printf '%s\n' '{"number":2,"title":"canceled","stateReason":"NOT_PLANNED","closedByPullRequestsReferences":{"nodes":[]}}' ;;
      3) printf '%s\n' '{"number":3,"title":"missing PR","stateReason":"COMPLETED","closedByPullRequestsReferences":{"nodes":[]}}' ;;
      4) printf '%s\n' '{"number":4,"title":"unmerged PR","stateReason":"COMPLETED","closedByPullRequestsReferences":{"nodes":[{"state":"OPEN"}]}}' ;;
      *) exit 2 ;;
    esac
    ;;
  *) exit 2 ;;
esac
FIXTURE
chmod +x "$fixture_dir/gh"
export PATH="$fixture_dir:$PATH"

check() {
  local scenario="$1" mode="$2" expected_status="$3" expected_count="$4"
  local output status=0
  local args=()
  if [[ -n "$mode" ]]; then args+=("$mode"); fi
  output="$(TRACKER_FIXTURE="$scenario" bash "$script_dir/check-tracker.sh" "${args[@]}")" || status=$?
  if [[ "$status" != "$expected_status" || "$output" != *"Violations (closed but no merged PR linked): $expected_count"* ]]; then
    printf 'FAIL %s %s: status=%s\n%s\n' "$scenario" "$mode" "$status" "$output" >&2
    exit 1
  fi
  if [[ "$output" == *'#2 canceled'* ]]; then
    echo 'FAIL canceled work counted as a violation' >&2
    exit 1
  fi
  if [[ "$scenario" == broken && ( "$output" != *'#3 missing PR'* || "$output" != *'#4 unmerged PR'* ) ]]; then
    echo 'FAIL completed work lost merged-PR enforcement' >&2
    exit 1
  fi
  printf 'PASS %s %s\n' "$scenario" "$mode"
}

check clean --strict 0 0
check broken --strict 1 2
check clean "" 0 0
check broken "" 0 2

for scenario in unavailable missing; do
  status=0
  output="$(TRACKER_FIXTURE="$scenario" bash "$script_dir/check-tracker.sh" --strict 2>&1)" || status=$?
  if [[ "$status" != 2 || "$output" != *'audit incomplete'* || "$output" == *'Violations'* ]]; then
    printf 'FAIL %s API response: status=%s\n%s\n' "$scenario" "$status" "$output" >&2
    exit 1
  fi
  printf 'PASS %s API response reports an incomplete audit\n' "$scenario"
done
