#!/usr/bin/env bash
set -euo pipefail

rendered="$(helm template squall deploy/helm/squall \
  --set-string dstack.adminToken.value=test \
  --set controller.env.dstackURL=http://dstack)"

grep -A2 -- '--metrics-bind-address=:8443' <<<"${rendered}" | grep -F -- '--metrics-secure=false' >/dev/null
grep -A2 'name: DSTACK_ENABLE_PROMETHEUS_METRICS' <<<"${rendered}" | grep -Fx '          value: "1"' >/dev/null
grep -F 'name: squall-controller-metrics' <<<"${rendered}" >/dev/null
[[ "$(grep -Fc 'kind: ServiceMonitor' <<<"${rendered}" || true)" -eq 0 ]]

monitored="$(helm template squall deploy/helm/squall \
  --set-string dstack.adminToken.value=test \
  --set controller.env.dstackURL=http://dstack \
  --set serviceMonitor.enabled=true)"

[[ "$(grep -Fc 'kind: ServiceMonitor' <<<"${monitored}")" -eq 3 ]]
grep -F 'name: squall-controller' <<<"${monitored}" >/dev/null
grep -F 'name: squall-proxy' <<<"${monitored}" >/dev/null
grep -F 'name: squall-dstack' <<<"${monitored}" >/dev/null
[[ "$(grep -Fc 'path: /metrics' <<<"${monitored}")" -eq 3 ]]

# Default (SQLite) install still gets the service-client timeout.
grep -A1 'name: DSTACK_SERVICE_CLIENT_TIMEOUT' <<<"${rendered}" | grep -Fx '          value: "600"' >/dev/null

# metricsBindAddress "0" disables metrics: no port, no flags.
off="$(helm template squall deploy/helm/squall \
  --set-string dstack.adminToken.value=test \
  --set controller.env.dstackURL=http://dstack \
  --set-string controller.metricsBindAddress=0)"
[[ "$(grep -Fc 'containerPort: 0' <<<"${off}" || true)" -eq 0 ]]
[[ "$(grep -Fc 'name: metrics' <<<"${off}" || true)" -eq 0 ]]
[[ "$(grep -Fc -- '--metrics-bind-address' <<<"${off}" || true)" -eq 0 ]]

# A host-qualified address takes its port from the last colon.
hostq="$(helm template squall deploy/helm/squall \
  --set-string dstack.adminToken.value=test \
  --set controller.env.dstackURL=http://dstack \
  --set-string controller.metricsBindAddress=127.0.0.1:8443)"
grep -F 'containerPort: 8443' <<<"${hostq}" >/dev/null
grep -Fx '    port: 8443' <<<"${hostq}" >/dev/null
