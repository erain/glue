#!/usr/bin/env bash
# Copyright (c) 2026 Yu YI
# SPDX-License-Identifier: MIT
# scripts/ax-dev/e2e/image-smoke.sh — check that the Glue task image ships the
# toolchains remote jobs rely on. Usage: image-smoke.sh [image]
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/../versions.env"
if [[ -z "${1:-}" && -f "${STATE}/images.env" ]]; then
  # shellcheck disable=SC1090,SC1091
  source "${STATE}/images.env"
fi
IMAGE="${1:-${GLUE_TASK_IMAGE:?pass an image or build one with scripts/ax-dev/build-task-image.sh}}"
docker run --rm --entrypoint bash "${IMAGE}" -euo pipefail -c '
  for c in "go version" "node --version" "npm --version" "python3 --version" "python3 -m venv --help" "uv --version" "gcc --version" "make --version" "git --version" "rg --version" "jq --version" "curl --version" "glue version"; do
    out=$($c 2>&1 | head -1) || { echo "FAIL  $c"; exit 1; }
    printf "ok    %-22s %s\n" "${c%% *}" "${out:0:60}"
  done'
printf 'size  %s MiB\n' "$(( $(docker image inspect --format='{{.Size}}' "${IMAGE}") / 1024 / 1024 ))"
