#!/usr/bin/env bash
# Copyright (c) 2026 Yu YI
# SPDX-License-Identifier: MIT
# scripts/ax-dev/env.sh — configure shell environment for the standing kind-glue-ax cluster
# without modifying ~/.kube/config. Usage:
#   source scripts/ax-dev/env.sh
#   eval "$(scripts/ax-dev/env.sh)"

if [[ -n "${BASH_SOURCE[0]:-}" ]]; then
  _glue_ax_env_file="${BASH_SOURCE[0]}"
elif [[ -n "${ZSH_VERSION:-}" ]]; then
  # shellcheck disable=SC2296
  _glue_ax_env_file="${(%):-%x}"
else
  _glue_ax_env_file="$0"
fi
_glue_ax_dev_dir="$(cd "$(dirname "${_glue_ax_env_file}")" && pwd)"

# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${_glue_ax_dev_dir}/versions.env"

if [[ -f "${STATE}/images.env" ]]; then
  # shellcheck disable=SC1090,SC1091
  source "${STATE}/images.env"
fi
export GLUE_TASK_IMAGE="${GLUE_TASK_IMAGE:-${REGISTRY_ADDR}/glue-task:dev}"

if [[ -n "${BASH_SOURCE[0]:-}" && "${BASH_SOURCE[0]}" == "${0}" ]]; then
  for _glue_ax_var in STATE KUBECONFIG KUBE_CONTEXT GLUE_AX_CONTEXT AX_SYSTEM_NAMESPACE GLUE_TASK_IMAGE PATH; do
    printf 'export %s=%q\n' "${_glue_ax_var}" "${!_glue_ax_var}"
  done
  for _glue_ax_var in AX_SERVER_IMAGE AX_TASK_RUNNER_IMAGE AX_DEFAULT_TASK_IMAGE; do
    if [[ -n "${!_glue_ax_var:-}" ]]; then
      printf 'export %s=%q\n' "${_glue_ax_var}" "${!_glue_ax_var}"
    fi
  done
  unset _glue_ax_var
fi

unset _glue_ax_env_file _glue_ax_dev_dir
