#!/usr/bin/env bash
# Install git hooks for squall (Phase 10.3).
#
# Installs:
#   pre-push   -> scripts/pre-push-check.sh
#     Runs the full pre-publication check before every `git push`.
#     Includes gitleaks/trufflehog/SPDX plus a defensive golangci-lint full
#     sweep + make test-unit.
#
# Idempotent — safe to re-run.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not inside a git repo" >&2; exit 1;
}
cd "$REPO_ROOT"

HOOKS_DIR="$(git rev-parse --git-path hooks)"
mkdir -p "$HOOKS_DIR"

install_hook() {
  local name="$1" target="$2"
  local hook_path="${HOOKS_DIR}/${name}"
  if [[ ! -x "scripts/${target##*/}" ]]; then
    echo "scripts/${target##*/} missing or not executable" >&2
    return 1
  fi
  # Backup any prior non-symlink hook.
  if [[ -e "$hook_path" && ! -L "$hook_path" ]]; then
    local backup="${hook_path}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
    mv "$hook_path" "$backup"
    echo "backed up existing $hook_path -> $backup"
  fi
  ln -sf "$target" "$hook_path"
  if [[ -L "$hook_path" ]]; then
    echo "installed: $hook_path -> $(readlink "$hook_path")"
  else
    echo "failed to install $hook_path" >&2
    return 1
  fi
}

install_hook pre-push "../../scripts/pre-push-check.sh"
