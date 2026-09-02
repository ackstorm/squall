#!/usr/bin/env bash
set -euo pipefail

rendered="$(helm template squall deploy/helm/squall \
  --set-string dstack.adminToken.value=test \
  --set controller.env.dstackURL=http://dstack)"

grep -F 'hostname: "${DSTACK_PROXY_JUMP_HOSTNAME}"' <<<"${rendered}" >/dev/null
grep -A4 'name: DSTACK_PROXY_JUMP_HOSTNAME' <<<"${rendered}" | grep -F 'fieldPath: status.hostIP' >/dev/null
grep -A5 '# Source: squall/templates/dstack-namespace.yaml' <<<"${rendered}" | grep -Fx '  name: squall' >/dev/null

secret_rendered="$(helm template squall deploy/helm/squall \
  --set dstack.adminToken.valueFrom.secretKeyRef.name=dstack-admin-token \
  --set dstack.adminToken.valueFrom.secretKeyRef.key=token \
  --set controller.env.dstackURL=http://dstack)"

[[ "$(grep -Fc 'name: dstack-admin-token' <<<"${secret_rendered}")" -eq 4 ]]
