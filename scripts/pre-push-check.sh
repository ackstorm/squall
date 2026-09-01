#!/usr/bin/env bash
# Pre-push validation for public-repo publication (Block 9+10+11, Phase 10.3).
# Run from anywhere inside the repo. Exits 0 if safe to push.
#
# Ported from ../alitellm-operator's scripts/pre-push-check.sh, trimmed to
# what squall's shape actually has today: no govulncheck ack-list infra and
# no go.mod-tidy-drift history of its own yet, so those gates are left out
# rather than invented from scratch (see docs/references/deviations-and-
# findings.md). Secret scanning and the internal-hostname/ackstorm-email
# checks are kept verbatim — the plan calls those out as mattering most.
#
# Hard checks (failure blocks push):
#   1. gitleaks       (only commits being pushed; full history on first push)
#   2. trufflehog     (only commits being pushed; full history on first push)
#   3. large tracked files (>2MB)
#   4. sensitive file patterns (.env, *.pem, *.key, kubeconfig, ...)
#   5. LICENSE + README presence
#   6. origin remote is the expected REPOSITORY (transport-insensitive:
#      git@host:owner/repo and https://host/owner/repo are the same thing;
#      pushing to a different repo is what this blocks)
#   7. internal hostnames / private IPv4 in tracked files (D36: hard-fail;
#      narrow, explicit exceptions live in .security-allowlist)
#   8. ackstorm.com/.ai/.es emails outside LICENSE/NOTICE/AUTHORS (D36:
#      hard-fail; exceptions in .security-allowlist)
#  10. go mod tidy drift
#  11. helm-sync drift (make helm-sync-check)
#  12. license-header SPDX gate (every in-scope *.go starts with the line)
#  13. golangci-lint full sweep (no pre-commit stage; this is the local gate)
#  14. make test-unit (pure-logic regression)
#
# Soft checks (warnings only):
#   9. .gitignore sanity (.env, .claude)
#  15. TODO/DO-NOT-COMMIT markers
#  16. uncommitted working-tree changes (informational)

set -uo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not inside a git repo" >&2; exit 1;
}
cd "$REPO_ROOT"

EXPECTED_REMOTE="git@github.com:ackstorm/squall.git"

if [[ -t 1 ]]; then
  RED=$'\033[31m'; YEL=$'\033[33m'; GRN=$'\033[32m'; BLU=$'\033[34m'; RST=$'\033[0m'
else
  RED=""; YEL=""; GRN=""; BLU=""; RST=""
fi

FAIL=0
WARN=0

hdr()  { printf '\n%s== %s ==%s\n' "$BLU" "$*" "$RST"; }
ok()   { printf '%sOK%s   %s\n' "$GRN" "$RST" "$*"; }
warn() { printf '%sWARN%s %s\n' "$YEL" "$RST" "$*"; WARN=$((WARN+1)); }
fail() { printf '%sFAIL%s %s\n' "$RED" "$RST" "$*"; FAIL=$((FAIL+1)); }

# --- preflight: docker available ---
if ! command -v docker >/dev/null 2>&1; then
  echo "${RED}docker is required for secret scanning.${RST}" >&2
  exit 2
fi
if ! docker info >/dev/null 2>&1; then
  echo "${RED}docker daemon not reachable.${RST}" >&2
  exit 2
fi

# --- scan scope: only new commits, full history on first push ---
BASE_REF="${PRE_PUSH_BASE_REF:-origin/main}"
if git rev-parse --verify "$BASE_REF" >/dev/null 2>&1; then
  SCAN_BASE=$(git merge-base "$BASE_REF" HEAD 2>/dev/null || git rev-parse "$BASE_REF")
  SCAN_LABEL="$BASE_REF..HEAD"
  GITLEAKS_LOG_OPTS="--log-opts=${SCAN_BASE}..HEAD"
  TRUFFLEHOG_SINCE="--since-commit=${SCAN_BASE}"
else
  SCAN_BASE=""
  SCAN_LABEL="full history (no $BASE_REF tracking ref — first push bootstrap)"
  GITLEAKS_LOG_OPTS=""
  TRUFFLEHOG_SINCE=""
fi

# --- worktree-safe scan root ---
# A git WORKTREE has a `.git` FILE (not a dir) pointing at an external
# gitdir the scanner containers cannot resolve. Scan a throwaway
# self-contained clone in that case (see alitellm-operator's original for
# the full rationale).
SCAN_ROOT="$REPO_ROOT"
SCAN_TMP=""
if [[ -f "$REPO_ROOT/.git" ]]; then
  SCAN_TMP="$(mktemp -d)"
  if git clone --quiet --no-hardlinks "$REPO_ROOT" "$SCAN_TMP/repo" 2>/dev/null; then
    SCAN_ROOT="$SCAN_TMP/repo"
  else
    warn "worktree clone for secret scan failed; scanning worktree dir directly (may under-scan)"
  fi
fi
trap '[[ -n "$SCAN_TMP" ]] && rm -rf "$SCAN_TMP"' EXIT

# --- allowlist (D36: narrow, explicit exceptions for checks 7/8) ---
SECURITY_ALLOWLIST="$REPO_ROOT/.security-allowlist"

# filter_allowed reads "file:line:content" hits on stdin and drops any
# whose file exactly matches a "<check>:<path>" entry in
# .security-allowlist. $1 is the check name (hostname|privip|email).
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

# --- 1. gitleaks ---
hdr "1. gitleaks ($SCAN_LABEL)"
if docker run --rm -v "$SCAN_ROOT:/repo:ro" zricethezav/gitleaks:latest \
     detect --source=/repo --redact --no-banner \
     --config=/repo/.gitleaks.toml $GITLEAKS_LOG_OPTS; then
  ok "no leaks detected"
else
  fail "gitleaks found secrets (see output above)"
fi

# --- 2. trufflehog ---
hdr "2. trufflehog ($SCAN_LABEL)"
if docker run --rm -v "$SCAN_ROOT:/pwd:ro" trufflesecurity/trufflehog:latest \
     git file:///pwd --only-verified --fail --no-update $TRUFFLEHOG_SINCE; then
  ok "no verified live secrets"
else
  fail "trufflehog found verified live secrets"
fi

# --- 3. Large tracked files ---
hdr "3. large tracked files (>2MB)"
LARGE=$(git ls-files -z | while IFS= read -r -d '' f; do
  [[ -f $f ]] || continue
  sz=$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f" 2>/dev/null || echo 0)
  if (( sz > 2097152 )); then
    printf '  %10d  %s\n' "$sz" "$f"
  fi
done)
if [[ -z $LARGE ]]; then
  ok "no tracked files over 2MB"
else
  fail "large tracked files:"
  printf '%s\n' "$LARGE"
fi

# --- 4. Sensitive file patterns ---
hdr "4. sensitive file patterns in tracked files"
SENSITIVE_PATTERNS=(
  '(^|/)\.env($|\..*)'
  '\.pem$' '\.key$' '\.pfx$' '\.p12$' '\.pkcs12$'
  '(^|/)id_rsa([^/]*)$' '(^|/)id_ed25519([^/]*)$' '(^|/)id_ecdsa([^/]*)$'
  '(^|/)credentials\.json$'
  '(^|/)kubeconfig$' '\.kubeconfig$'
  '(^|/)service-account.*\.json$'
  '(^|/)gcp-key.*\.json$'
  '(^|/)aws-credentials($|\..*)'
  '(^|/)\.npmrc$' '(^|/)\.pypirc$'
)
SENS_HITS=""
for pat in "${SENSITIVE_PATTERNS[@]}"; do
  m=$(git ls-files | grep -E "$pat" || true)
  [[ -n $m ]] && SENS_HITS+="$m"$'\n'
done
if [[ -z $SENS_HITS ]]; then
  ok "no sensitive file patterns tracked"
else
  fail "sensitive file patterns tracked:"
  printf '%s' "$SENS_HITS"
fi

# --- 5. LICENSE + README ---
hdr "5. LICENSE + README presence"
[[ -f LICENSE ]]   && ok "LICENSE present"   || fail "LICENSE missing"
[[ -f README.md ]] && ok "README.md present" || fail "README.md missing"

# --- 6. Remote check ---
hdr "6. origin remote"
# Normalise a remote URL to "host/owner/repo". The thing worth blocking is a
# push to the WRONG REPOSITORY; typing https:// where the team writes git@ is
# not that, and failing on it made a hard check look like a broken one --
# which is how hard checks end up routinely bypassed with --no-verify.
normalise_remote() {
  printf '%s' "$1" \
    | sed -E 's#^git\+ssh://#ssh://#; s#^ssh://##; s#^https?://##; s#^[^/@]+@##; s#^([^:/]+):#\1/#; s#\.git$##; s#/+$##' \
    | tr 'A-Z' 'a-z'
}

ACTUAL=$(git remote get-url origin 2>/dev/null || echo "")
if [[ -z $ACTUAL ]]; then
  warn "no 'origin' remote configured"
  echo "  expected: $EXPECTED_REMOTE"
  echo "  add with: git remote add origin $EXPECTED_REMOTE"
elif [[ $(normalise_remote "$ACTUAL") != "$(normalise_remote "$EXPECTED_REMOTE")" ]]; then
  fail "origin is '$ACTUAL', which is a different repository from '$EXPECTED_REMOTE'"
else
  ok "origin = $ACTUAL"
fi

# --- 7. Internal hostnames / private IPs ---
hdr "7. internal hostnames / private IPv4"
INTERNAL_RE='(ackstorm\.internal|\.ackstorm\.local|jira\.ackstorm|confluence\.ackstorm|gitlab\.ackstorm)'
PRIVIP_RE='(^|[^0-9.])(10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+|192\.168\.[0-9]+\.[0-9]+)'
INT_HITS=$(git grep -EnI "$INTERNAL_RE" -- ':!*.lock' ':!go.sum' ':!*.svg' 2>/dev/null | filter_allowed hostname || true)
IP_HITS=$(git grep -EnI  "$PRIVIP_RE"   -- ':!*.lock' ':!go.sum' ':!*.svg' 2>/dev/null | filter_allowed privip || true)
if [[ -z $INT_HITS && -z $IP_HITS ]]; then
  ok "no internal hostname/private-IP matches"
else
  if [[ -n $INT_HITS ]]; then
    fail "internal hostnames found (add a justified entry to .security-allowlist if legitimate):"
    printf '%s\n' "$INT_HITS" | head -20
  fi
  if [[ -n $IP_HITS ]]; then
    fail "private IPv4 found (add a justified entry to .security-allowlist if legitimate):"
    printf '%s\n' "$IP_HITS" | head -20
  fi
fi

# --- 8. Personal/company email leak ---
hdr "8. ackstorm emails in tracked files"
MAIL_HITS=$(git grep -EnI '[a-zA-Z0-9._%+-]+@(ackstorm\.com|ackstorm\.ai|ackstorm\.es)' \
              -- ':!LICENSE' ':!NOTICE' ':!AUTHORS' ':!CONTRIBUTORS*' 2>/dev/null | filter_allowed email || true)
if [[ -z $MAIL_HITS ]]; then
  ok "no ackstorm emails in code"
else
  fail "ackstorm emails in tracked files (add a justified entry to .security-allowlist if legitimate):"
  printf '%s\n' "$MAIL_HITS" | head -20
fi

# --- 9. .gitignore sanity ---
hdr "9. .gitignore sanity"
if [[ ! -f .gitignore ]]; then
  warn ".gitignore missing"
else
  for p in '.env' '.claude'; do
    if grep -qE "(^|/)${p//./\\.}(/|$)" .gitignore; then
      ok ".gitignore covers $p"
    else
      warn ".gitignore does not mention $p"
    fi
  done
fi

# --- 10. go mod tidy drift ---
hdr "10. go mod tidy drift"
# Snapshot go.mod / go.sum BEFORE tidy so we can restore them on drift —
# pre-push must not mutate the working tree.
SNAP_DIR=$(mktemp -d)
trap 'rm -rf "$SNAP_DIR" "$SCAN_TMP" 2>/dev/null' EXIT
cp go.mod "$SNAP_DIR/go.mod" 2>/dev/null || true
cp go.sum "$SNAP_DIR/go.sum" 2>/dev/null || true
if ./scripts/dev.sh go mod tidy >/tmp/gomod-tidy.txt 2>&1; then
  if git diff --quiet -- go.mod go.sum 2>/dev/null; then
    ok "go.mod / go.sum are tidy"
  else
    fail "go mod tidy produced uncommitted drift in go.mod / go.sum"
    git --no-pager diff -- go.mod go.sum | head -40
    [[ -f "$SNAP_DIR/go.mod" ]] && cp "$SNAP_DIR/go.mod" go.mod
    [[ -f "$SNAP_DIR/go.sum" ]] && cp "$SNAP_DIR/go.sum" go.sum
  fi
else
  fail "go mod tidy exited non-zero (see /tmp/gomod-tidy.txt)"
  sed -n '1,20p' /tmp/gomod-tidy.txt
fi

# --- 11. helm-sync (CRD chart) drift ---
hdr "11. helm-sync drift (config/crd/bases -> deploy/helm/squall/)"
# make helm-sync-check already snapshots nothing and mutates the working
# tree to match config/crd/bases/ before diffing — its own failure message
# names the fix. Delegate directly rather than duplicating its logic here.
if ./scripts/dev.sh make helm-sync-check >/tmp/helm-sync-check.txt 2>&1; then
  ok "deploy/helm/squall/ in sync with config/crd/bases/"
else
  fail "make helm-sync-check failed — run 'make helm-sync' and commit"
  sed -n '1,40p' /tmp/helm-sync-check.txt
fi

# --- 12. license-header SPDX gate ---
hdr "12. license-header SPDX gate"
# Every in-scope *.go file MUST carry `// SPDX-License-Identifier: Apache-2.0`
# on its first non-blank, non-build-tag line. Exempt: zz_generated*, mock_*.
MISSING_SPDX=""
while IFS= read -r f; do
  [[ "$(basename "$f")" == zz_generated* ]] && continue
  [[ "$(basename "$f")" == mock_* ]] && continue
  if ! head -5 "$f" 2>/dev/null | grep -qx "// SPDX-License-Identifier: Apache-2.0"; then
    MISSING_SPDX+="  $f"$'\n'
  fi
done < <(git ls-files '*.go')
if [[ -z $MISSING_SPDX ]]; then
  ok "every in-scope *.go file carries the SPDX header"
else
  fail "files missing SPDX header:"
  printf '%s' "$MISSING_SPDX" | head -20
fi

# --- 13. golangci-lint full sweep ---
hdr "13. golangci-lint full sweep"
if [[ -x scripts/dev.sh ]]; then
  if ./scripts/dev.sh make qa-lint >/tmp/pre-push-lint.log 2>&1; then
    ok "golangci-lint clean"
  else
    fail "golangci-lint reported issues — see /tmp/pre-push-lint.log"
  fi
else
  warn "scripts/dev.sh missing — skipping lint gate (rebuild devtools image)"
fi

# --- 14. unit tests ---
hdr "14. unit tests"
if [[ -x scripts/dev.sh ]]; then
  if ./scripts/dev.sh make test-unit >/tmp/pre-push-unit.log 2>&1; then
    ok "make test-unit clean"
  else
    fail "make test-unit failed — see /tmp/pre-push-unit.log"
  fi
else
  warn "scripts/dev.sh missing — skipping unit gate (rebuild devtools image)"
fi

# --- 15. Urgent TODO markers ---
hdr "15. urgent TODO / DO-NOT-COMMIT markers"
TODO_HITS=$(git grep -nE '(DO[ _-]?NOT[ _-]?COMMIT|XXX[ _-]?REMOVE|TODO:?[ ]?(remove|delete|drop|secret))' \
              -- ':!scripts/pre-push-check.sh' 2>/dev/null || true)
if [[ -z $TODO_HITS ]]; then
  ok "no urgent TODO markers"
else
  warn "urgent TODO markers found:"
  printf '%s\n' "$TODO_HITS"
fi

# --- 16. Uncommitted changes ---
hdr "16. working tree status"
DIRTY=$(git status --porcelain)
if [[ -z $DIRTY ]]; then
  ok "working tree clean"
else
  warn "uncommitted changes present — they will NOT be pushed:"
  printf '%s\n' "$DIRTY" | head -20
fi

# --- Summary ---
printf '\n%s== Summary ==%s\n' "$BLU" "$RST"
printf 'Failures: %d\n' "$FAIL"
printf 'Warnings: %d\n' "$WARN"
echo
if (( FAIL > 0 )); then
  printf '%sBLOCKED: fix failures before pushing.%s\n' "$RED" "$RST"
  exit 1
fi
if (( WARN > 0 )); then
  printf '%sWarnings present — review each before pushing.%s\n' "$YEL" "$RST"
fi
printf '%sAll hard checks passed.%s\n' "$GRN" "$RST"
exit 0
