#!/usr/bin/env bash
set -euo pipefail

rendered="$(helm template squall deploy/helm/squall \
  --set dstack.adminToken=test \
  --set controller.env.dstackURL=http://dstack)"

grep -F 'hostname: "${DSTACK_PROXY_JUMP_HOSTNAME}"' <<<"${rendered}" >/dev/null
grep -A4 'name: DSTACK_PROXY_JUMP_HOSTNAME' <<<"${rendered}" | grep -F 'fieldPath: status.hostIP' >/dev/null
