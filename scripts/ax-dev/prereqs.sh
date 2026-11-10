#!/usr/bin/env bash
# scripts/ax-dev/prereqs.sh — verify host prerequisites for the Glue AX kind cluster.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ax-dev/versions.env disable=SC1091
source "${SCRIPT_DIR}/versions.env"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ok() { printf '  [ok] %s\n' "$*"; }
warn() { printf '  [warn] %s\n' "$*" >&2; }

printf 'Checking Glue AX kind cluster prerequisites...\n'

command -v docker >/dev/null 2>&1 || fail "docker CLI not found. Install Docker Engine (https://docs.docker.com/engine/install/)."
docker info >/dev/null 2>&1 || fail "docker daemon is not reachable. Start dockerd and add your user to the 'docker' group."
ok "Docker daemon reachable ($(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown))"

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found. Install kubectl (https://kubernetes.io/docs/tasks/tools/)."
ok "kubectl installed ($(kubectl version --client -o yaml 2>/dev/null | awk '/gitVersion:/ && !p++ {print $2}'))"

command -v go >/dev/null 2>&1 || fail "go not found. Install Go (https://go.dev/dl/); GOTOOLCHAIN=auto downloads ${GO_TOOLCHAIN}."
go_ver="$(GOTOOLCHAIN="${GO_TOOLCHAIN}" go version 2>/dev/null)" || fail "Go could not download or run ${GO_TOOLCHAIN} with GOTOOLCHAIN=auto."
[[ "${go_ver}" == *"go1.27."* ]] || fail "Expected Go 1.27.x from GOTOOLCHAIN=${GO_TOOLCHAIN}, got: ${go_ver}"
ok "Go toolchain ready (${go_ver})"

krel="$(uname -r)"
kmaj="${krel%%.*}"; krest="${krel#*.}"; kmin="${krest%%[^0-9]*}"
(( kmaj > 6 || (kmaj == 6 && kmin >= 8) )) ||
  fail "Linux kernel ${krel} is too old; >= 6.8 is required for OverlayFS lowerdir+ in gVisor sandboxes."
ok "Linux kernel ${krel} (>= 6.8 for OverlayFS lowerdir+)"

if [[ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null || true)" != "cgroup2fs" ]] && [[ ! -f /sys/fs/cgroup/cgroup.controllers ]]; then
  fail "cgroup v2 is required (/sys/fs/cgroup is not cgroup2fs). Boot with systemd.unified_cgroup_hierarchy=1."
fi
ok "cgroup v2 unified hierarchy mounted at /sys/fs/cgroup"

mkdir -p "${STATE}"
free_gb="$(df -PB1G "${STATE}" | awk 'NR==2 {gsub(/G/, "", $4); print $4}')"
if [[ -z "${free_gb}" ]] || (( free_gb < 40 )); then
  fail "Only ${free_gb:-0} GB free at ${STATE}; at least 40 GB is required for images, Go cache, and kind."
fi
ok "Disk space ${free_gb} GB free at ${STATE} (>= 40 GB)"

if [[ -e /dev/kvm && -r /dev/kvm && -w /dev/kvm ]]; then
  ok "/dev/kvm accessible (optional; microVM runtime supported)"
else
  warn "/dev/kvm missing or not read/write for $(id -un) (optional; microVM only, gVisor works without it)"
fi

inotify_max="$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0)"
if (( inotify_max < 8192 )); then
  warn "Host fs.inotify.max_user_instances=${inotify_max} (< 8192). up.sh sets 8192 in kind; host fix: sudo sysctl -w fs.inotify.max_user_instances=8192"
else
  ok "Host fs.inotify.max_user_instances=${inotify_max} (>= 8192)"
fi
