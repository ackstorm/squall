{{/*
SPDX-License-Identifier: Apache-2.0

squall.dstackTokenEnv renders the SQUALL_DSTACK_TOKEN environment variable for
BOTH deployments that need it — squall-controller (dstack's REST API) and
squall-proxy (dstack's service proxy, LIVE-4). One token, one dstack project,
so one definition rather than two copies that can drift apart.

Prefers a Secret reference. A literal `value:` puts the credential into the
Deployment object itself, where it is readable by anything that can `get
deployments` (a strictly wider audience than `get secrets`), and echoed back
by `kubectl describe`, `helm get values` and every GitOps diff. The literal is
kept only as the fallback the e2e fixtures use, where the token is a fixed,
obviously-fake string.

dstack's own storage of this credential is a separate problem, already
recorded: D47 (unencrypted at rest in dstack's DB) and D85 (returned in
cleartext by its config_info endpoint). Referencing a Secret here does not fix
those; it fixes only this chart's contribution.
*/}}
{{- define "squall.dstackTokenEnv" -}}
- name: SQUALL_DSTACK_TOKEN
{{- if .Values.controller.env.dstackTokenSecret.name }}
  valueFrom:
    secretKeyRef:
      name: {{ .Values.controller.env.dstackTokenSecret.name | quote }}
      key: {{ required "controller.env.dstackTokenSecret.key is required when dstackTokenSecret.name is set" .Values.controller.env.dstackTokenSecret.key | quote }}
{{- else }}
  value: {{ .Values.controller.env.dstackToken | quote }}
{{- end }}
{{- end -}}
