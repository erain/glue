#!/usr/bin/env bash
# scripts/check-tracker.sh — verify every closed issue has a merged PR.
#
# The Glue contributor protocol (CONTRIBUTING.md) requires "no issue is
# closed without a merged PR that references it." This script audits
# that invariant: for each closed issue in the repo, it queries GitHub
# for the closing-PR linkage and reports any closed issue whose
# closedByPullRequestsReferences does not include a merged PR.
#
# By default the script is dry-run only — it prints a report and exits 0
# even when violations are found. Pass --strict to make violations exit
# non-zero so the script can be wired into CI.
#
# Usage:
#   scripts/check-tracker.sh                  # report-only, exit 0
#   scripts/check-tracker.sh --strict         # fail on any violation
#   scripts/check-tracker.sh --limit 100      # cap closed issues fetched
#
# Requires: gh, jq.
set -euo pipefail

strict=false
limit=200
while [[ $# -gt 0 ]]; do
  case "$1" in
    --strict) strict=true ;;
    --limit) shift; limit="$1" ;;
    -h|--help)
      sed -n '2,/^set/p' "$0" | sed 's/^# \{0,1\}//;/^set/d'
      exit 0
      ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

if ! command -v gh >/dev/null; then
  echo "error: gh is required" >&2
  exit 2
fi
if ! command -v jq >/dev/null; then
  echo "error: jq is required" >&2
  exit 2
fi

# 1. List closed issue numbers (not PRs).
numbers="$(gh issue list --state closed --limit "$limit" --json number --jq '.[].number')"

# 2. For each one, fetch the closing-PR linkage. The
#    closedByPullRequestsReferences field is on `gh issue view`, not
#    `gh issue list`.
violations=()
total=0
for n in $numbers; do
  total=$((total + 1))
  view="$(gh issue view "$n" --json number,title,closedByPullRequestsReferences 2>/dev/null || true)"
  [[ -z "$view" ]] && continue

  merged_count="$(jq '
    (.closedByPullRequestsReferences // []) | map(select(.state == "MERGED")) | length
  ' <<<"$view")"
  if [[ "$merged_count" -eq 0 ]]; then
    title="$(jq -r '.title' <<<"$view")"
    violations+=("#$n $title")
  fi
done

echo "Closed issues scanned: $total"
echo "Violations (closed but no merged PR linked): ${#violations[@]}"
if [[ "${#violations[@]}" -gt 0 ]]; then
  printf '\n%s\n' "${violations[@]}"
fi

if [[ "$strict" == "true" ]] && [[ "${#violations[@]}" -gt 0 ]]; then
  exit 1
fi
exit 0
