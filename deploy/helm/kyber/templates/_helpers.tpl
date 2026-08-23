{{/*
Kyber Helm chart helpers.
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "kyber.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Labels that identify a pod as part of the Kyber logging/discovery boundary.
Keep this separate from selectorLabels: adding a label to an existing workload
selector is an immutable-field upgrade failure, while pod-template labels roll
safely. Every Kyber-owned pod template and controller-built pod includes this.
*/}}
{{- define "kyber.podLabels" -}}
app.kubernetes.io/part-of: kyber
{{- end }}

{{/* Resolve and validate the effective log level for a component. */}}
{{- define "kyber.loggingLevel" -}}
{{- $logging := default (dict) .root.Values.logging -}}
{{- $components := default (dict) $logging.components -}}
{{- $level := default "info" $logging.level -}}
{{- if hasKey $components .component -}}
{{- $component := index $components .component -}}
{{- $level = default $level $component.level -}}
{{- end -}}
{{- if not (has $level (list "debug" "info" "warn" "error")) -}}
{{- fail (printf "invalid logging level %q for component %q: want debug, info, warn, or error" $level .component) -}}
{{- end -}}
{{- $level -}}
{{- end }}

{{/* Upgrade-safe logging settings for releases whose reused values predate logging. */}}
{{- define "kyber.loggingComponentsJSON" -}}
{{- $logging := default (dict) .Values.logging -}}
{{- default (dict) $logging.components | toJson -}}
{{- end }}

{{- define "kyber.loggingRetentionDays" -}}
{{- $logging := default (dict) .Values.logging -}}
{{- $archive := default (dict) $logging.archive -}}
{{- default 30 $archive.retentionDays -}}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this
(by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "kyber.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "kyber.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to all resources.
*/}}
{{- define "kyber.labels" -}}
helm.sh/chart: {{ include "kyber.chart" . }}
{{ include "kyber.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (subset used in matchLabels).
*/}}
{{- define "kyber.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kyber.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Render an image reference: repo:tag. The `tag` value accepts either `vX.Y.Z` or
the canonical OCI `vX.Y.Z@sha256:...` digest-pinned form — the simple printf
passes both through unchanged so kubelet/containerd resolve by digest with the
tag retained as metadata. Digest pinning is written automatically by the release
pipeline's deploy-bump-pr job (kyber#364) to make the rendered manifest
reference the exact bytes.

The tag is REQUIRED — we deliberately do NOT fall back to Chart.AppVersion.
Chart.AppVersion tracks the chart/release version, not a per-image tag: a missing
pin used to render an unpullable `:0.1.0` reference (the old placeholder). For
agent images that 404 was then enforced fleet-wide by the sidecar-image
convergence loop (kyber#358), which deleted running agent pods to "converge" them
onto the phantom image — silently killing agents mid-session (R2-D2 crash
investigation 2026-05-29). Since kyber#457 appVersion is a REAL published release
(e.g. 1.8.0), so an accidental fallback would now resolve to a real-looking but
wrong tag SILENTLY instead of 404'ing — which is why the pin stays REQUIRED.
Failing the render loudly keeps a silent fleet outage from ever recurring. Pins
come from ArgoCD Image Updater, release.yml, or an explicit --set.
Usage: {{ include "kyber.image" (dict "repo" .Values.image.controlPlane.repository "tag" .Values.image.controlPlane.tag) }}
*/}}
{{- define "kyber.image" -}}
{{- $tag := required (printf "image tag for %q must be set — pin it via ArgoCD Image Updater, release.yml, or --set. Refusing the Chart.AppVersion fallback: it is the chart/release version, not a per-image tag (kyber#358, #457)." .repo) .tag -}}
{{- printf "%s:%s" .repo $tag -}}
{{- end }}

{{/*
Return the name of the Secret holding API credentials.
When api.existingSecret is set the operator manages the secret; otherwise the chart
creates one named <fullname>-api-credentials.
*/}}
{{- define "kyber.secretName" -}}
{{- if .Values.api.existingSecret -}}
{{- .Values.api.existingSecret -}}
{{- else -}}
{{- include "kyber.fullname" . }}-api-credentials
{{- end -}}
{{- end }}

{{/*
Return the name of the Anthropic-key Secret used by the runtime detection
poller (kyber#375 PR-A). When runtimeDetect.existingSecret is set the
operator manages the secret; otherwise the chart creates
<fullname>-anthropic-key with an empty api-key field, which the PWA
Settings panel fills in via PUT /api/v1/settings/anthropic-key.
*/}}
{{- define "kyber.anthropicKeySecretName" -}}
{{- if .Values.runtimeDetect.existingSecret -}}
{{- .Values.runtimeDetect.existingSecret -}}
{{- else -}}
{{- include "kyber.fullname" . }}-anthropic-key
{{- end -}}
{{- end }}

{{/*
Return the name of the k3s Secret.
When k3s.existingSecret is set the operator manages the secret; otherwise the chart
stores k3s config in the api-credentials secret (same secret, extra keys).
*/}}
{{- define "kyber.k3sSecretName" -}}
{{- if .Values.k3s.existingSecret -}}
{{- .Values.k3s.existingSecret -}}
{{- else -}}
{{- include "kyber.secretName" . -}}
{{- end -}}
{{- end }}

{{/*
Return the namespace where resources should be created.
Defaults to .Values.namespace.name, which is kyber-system.
*/}}
{{- define "kyber.namespace" -}}
{{- .Values.namespace.name | default .Release.Namespace }}
{{- end }}

{{/*
Return the name of the Postgres credential Secret.
When postgresql.auth.existingSecret is set the operator manages the secret
(it must contain a `postgres-password` key); otherwise the chart creates
{{ .Release.Name }}-postgres and populates it with auth.password (or a
randAlphaNum fallback stabilised by `lookup`).

Recommend existingSecret for ArgoCD installs: ArgoCD's repo-server renders
templates without cluster access, so `lookup` returns empty and the
randAlphaNum fallback regenerates on every sync — the live StatefulSet pod
still has the first-generated password and fails auth.
*/}}
{{- define "kyber.postgresSecretName" -}}
{{- if .Values.postgresql.auth.existingSecret -}}
{{- .Values.postgresql.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-postgres" .Release.Name -}}
{{- end -}}
{{- end }}

{{/*
Return the name of the preview credentials Secret (api-key +
k3s config) created by the bootstrap Job.
When preview.credentialsSecretName is set use that; otherwise default to
<release-name>-credentials.
*/}}
{{- define "kyber.previewCredentialsSecretName" -}}
{{- if .Values.preview.credentialsSecretName -}}
{{- .Values.preview.credentialsSecretName -}}
{{- else -}}
{{- printf "%s-credentials" .Release.Name -}}
{{- end -}}
{{- end }}

{{/*
Return the name of the preview postgres Secret created by the bootstrap Job.
When preview.postgresSecretName is set use that; otherwise default to
<release-name>-postgres.
*/}}
{{- define "kyber.previewPostgresSecretName" -}}
{{- if .Values.preview.postgresSecretName -}}
{{- .Values.preview.postgresSecretName -}}
{{- else -}}
{{- printf "%s-postgres" .Release.Name -}}
{{- end -}}
{{- end }}
