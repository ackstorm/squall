#!/usr/bin/env bash
# scripts/security-scan.sh — the CI-runnable half of D36's publication
# backstop: secret scanning (gitleaks, trufflehog) plus the internal-
# hostname/private-IP and ackstorm-email sweeps, over the FULL repo history.
#
# This is intentionally the same category of checks scripts/pre-push-check.sh
# runs locally (checks 1/2/7/8 there), against the same .security-allowlist —
# but scoped to the whole repo/history rather than just the commits being
# pushed, because CI has no "what's new since origin/main" framing and this
# is meant to be the backstop for a hook nobody is required to install.
#
# Usage: ./scripts/security-scan.sh   (run via `make qa-security`, which
# already routes through scripts/dev.sh so docker/git are available).
# Exits 0 if clean, 1 if any check fails.

set -uo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not inside a git repo" >&2; exit 1;
}
cd "$REPO_ROOT"

if [[ -t 1 ]]; then
  RED=$'\033[31m'; GRN=$'\033[32m'; BLU=$'\033[34m'; RST=$'\033[0m'
else
  RED=""; GRN=""; BLU=""; RST=""
fi

FAIL=0
hdr()  { printf '\n%s== %s ==%s\n' "$BLU" "$*" "$RST"; }
ok()   { printf '%sOK%s   %s\n' "$GRN" "$RST" "$*"; }
fail() { printf '%sFAIL%s %s\n' "$RED" "$RST" "$*"; FAIL=$((FAIL+1)); }

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "${RED}docker is required for secret scanning.${RST}" >&2
  exit 2
fi

# This script normally runs INSIDE the devtools container (`make
# qa-security` -> scripts/dev.sh), which mounts the HOST's docker.sock so it
# can launch gitleaks/trufflehog as SIBLING containers on the host daemon —
# not nested ones. That daemon resolves `docker run -v <src>:...` against
# the HOST filesystem, so <src> must be the HOST path to this checkout, not
# this container's own /workspace view of it. scripts/dev.sh exports
# HOST_PWD for exactly this; fall back to REPO_ROOT when run directly on a
# host with a real docker (no devtools container in between).
SCAN_ROOT="${HOST_PWD:-$REPO_ROOT}"

# --- 1. gitleaks (full history) ---
hdr "gitleaks (full history)"
if docker run --rm -v "$SCAN_ROOT:/repo:ro" zricethezav/gitleaks:latest \
     detect --source=/repo --redact --no-banner --config=/repo/.gitleaks.toml; then
  ok "no leaks detected"
else
  fail "gitleaks found secrets (see output above)"
fi

# --- 2. trufflehog (full history, verified live secrets only) ---
hdr "trufflehog (full history)"
if docker run --rm -v "$SCAN_ROOT:/pwd:ro" trufflesecurity/trufflehog:latest \
     git file:///pwd --only-verified --fail --no-update; then
  ok "no verified live secrets"
else
  fail "trufflehog found verified live secrets"
fi

# --- 3/4. internal hostnames / private IPv4 / ackstorm emails ---
# Same regexes and allowlist as pre-push-check.sh's checks 7/8 (D36); see
# .security-allowlist for the narrow, justified exceptions.
SECURITY_ALLOWLIST="$REPO_ROOT/.security-allowlist"
filter_allowed() {
  local check="$1" allowed
  allowed=$(grep -E "^${check}:" "$SECURITY_ALLOWLIST" 2>/dev/null | cut -d: -f2-)
  while IFS= read -r line; do
    [[ -z $line ]] && continue
    if [[ -n $allowed ]] && grep -qxF "${line%%:*}" <<<"$allowed"; then
      continue
    fi
    printf '%s\n' "$line"
  done
}

hdr "internal hostnames / private IPv4"
INTERNAL_RE='(ackstorm\.internal|\.ackstorm\.local|jira\.ackstorm|confluence\.ackstorm|gitlab\.ackstorm)'
PRIVIP_RE='(^|[^0-9.])(10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+|192\.168\.[0-9]+\.[0-9]+)'
INT_HITS=$(git grep -EnI "$INTERNAL_RE" -- ':!*.lock' ':!go.sum' ':!*.svg' 2>/dev/null | filter_allowed hostname || true)
IP_HITS=$(git grep -EnI  "$PRIVIP_RE"   -- ':!*.lock' ':!go.sum' ':!*.svg' 2>/dev/null | filter_allowed privip || true)
if [[ -z $INT_HITS && -z $IP_HITS ]]; then
  ok "no internal hostname/private-IP matches"
else
  [[ -n $INT_HITS ]] && { fail "internal hostnames found:"; printf '%s\n' "$INT_HITS" | head -20; }
  [[ -n $IP_HITS ]]  && { fail "private IPv4 found:"; printf '%s\n' "$IP_HITS" | head -20; }
fi

hdr "ackstorm emails in tracked files"
MAIL_HITS=$(git grep -EnI '[a-zA-Z0-9._%+-]+@(ackstorm\.com|ackstorm\.ai|ackstorm\.es)' \
              -- ':!LICENSE' ':!NOTICE' ':!AUTHORS' ':!CONTRIBUTORS*' 2>/dev/null | filter_allowed email || true)
if [[ -z $MAIL_HITS ]]; then
  ok "no ackstorm emails in code"
else
  fail "ackstorm emails in tracked files:"
  printf '%s\n' "$MAIL_HITS" | head -20
fi

# --- Summary ---
printf '\n%s== Summary ==%s\n' "$BLU" "$RST"
printf 'Failures: %d\n' "$FAIL"
if (( FAIL > 0 )); then
  printf '%sBLOCKED: security-scan found issues (add a justified .security-allowlist entry if a hit is legitimate).%s\n' "$RED" "$RST"
  exit 1
fi
printf '%sAll security checks passed.%s\n' "$GRN" "$RST"
exit 0
