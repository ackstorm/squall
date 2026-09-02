# Metrics

Squall exposes unauthenticated Prometheus text endpoints inside the cluster:

| Component | Service | Endpoint |
|---|---|---|
| controller | `squall-controller-metrics.squall-system:8443` | `/metrics` |
| proxy | `squall-proxy.squall-system:8080` | `/metrics` |
| bundled dstack | `dstack.squall-system:3000` | `/metrics` |

All Services are `ClusterIP`. If they are exposed beyond the cluster network,
put authentication and transport security in front of them.

## Prometheus Operator

The chart does not require the Prometheus Operator. Enable its three
`ServiceMonitor` resources when the CRD is installed:

```yaml
serviceMonitor:
  enabled: true
  labels: {}          # for example: {release: kube-prometheus-stack}
  interval: 30s
  scrapeTimeout: 10s
```

## Squall metrics

Controller model gauges use Kubernetes object names and controlled
`backend`, `fleet`, `state`, `phase`, and `reason` values. Gauges disappear
after the Model is deleted. Process-local counters and histograms retain a
deleted Model's labels until the controller restarts, when all of them reset.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `squall_model_phase` | gauge | `namespace,name,phase` | Current lifecycle phase (one series with value 1) |
| `squall_model_run_active` | gauge | `namespace,name` | Whether dstack reports a live run |
| `squall_model_replicas` | gauge | `namespace,name` | Current observed replica count |
| `squall_model_fleet_state` | gauge | `namespace,name,backend,fleet,state` | Fleet admission state; `fleet` is empty when dstack cannot identify it |
| `squall_model_provisioning_failure` | gauge | `namespace,name,backend,reason` | Latest unresolved failure; reason is `no_capacity`, `insufficient_credit`, `rate_limited`, or `other` |
| `squall_model_transitions_total` | counter | `namespace,name,backend,transition` | `wake`, `sleep`, `recreate`, and `delete` transitions |
| `squall_model_provisioning_attempts_total` | counter | `namespace,name,backend` | Scale-up attempts |
| `squall_model_provisioning_outcomes_total` | counter | `namespace,name,backend,outcome,reason` | `success` and `failure` outcomes |
| `squall_model_provisioning_duration_seconds` | histogram | `namespace,name,backend,outcome` | Time from wake actuation to outcome |
| `squall_model_age_seconds` / `squall_model_max_lifetime_seconds` | gauges | `namespace,name` | Observed and declared lifetime |
| `squall_model_price_per_hour` / `squall_model_max_price_per_hour` | gauges | `namespace,name` | Observed and declared hourly price |
| `squall_model_uncontrolled_seconds` / `squall_model_uncontrolled_timeout_seconds` | gauges | `namespace,name` | Capacity without reliable idle evidence and its deadline |

Controller-runtime also exports reconcile counts and latency as
`controller_runtime_reconcile_total` and
`controller_runtime_reconcile_time_seconds`; Squall does not duplicate them.

Proxy labels use the requested model name only after it matches a Model CR;
unrecognised input is collapsed to `_unknown` to prevent unbounded series.
All proxy series are process-local and reset when that proxy replica restarts.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `squall_proxy_requests_total` | counter | `model,outcome,status_code,held` | Requests by result and HTTP status; `held` records whether they waited for wake-up |
| `squall_proxy_request_duration_seconds` | histogram | `model,outcome,held` | End-to-end request latency, including wake waits |
| `squall_proxy_requests_in_flight` | gauge | `model` | Accepted requests currently running |
| `squall_proxy_held_requests` | gauge | `model` | Requests waiting for a model to wake |

Proxy `outcome` is one of `forwarded`, `rejected`, `failed`, or `client_gone`.
A client disconnect before a response uses status code `0`.

## dstack metrics

The chart enables dstack's native Prometheus endpoint with
`DSTACK_ENABLE_PROMETHEUS_METRICS=1`. Set `dstack.metrics.enabled: false` to
disable it (and omit its `ServiceMonitor`). It includes run, job, instance,
price, resource, provisioning-duration, and pending-run metrics. Several
native dstack series contain run, job, or instance IDs and therefore have
higher cardinality than Squall's metrics; apply retention or relabeling rules
appropriate to your Prometheus installation.

dstack does not export offer considered/accepted/rejected counts through this
endpoint. Squall cannot publish those truthfully without adding dstack's
`get_plan` offer-selection API to the reconciliation path, so they remain
deferred rather than being inferred from provisioning outcomes.

dstack's optional OpenTelemetry HTTP-server metrics are intentionally not
enabled. The native endpoint covers the operational signals requested here;
enable additional dstack telemetry separately if request-level server health
is needed. See [dstack's metrics documentation](https://dstack.ai/docs/concepts/metrics/).
