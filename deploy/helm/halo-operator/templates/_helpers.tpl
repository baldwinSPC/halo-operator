{{/* Chart name, overridable by the release. */}}
{{- define "halo-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified release name. */}}
{{- define "halo-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "halo-operator.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "halo-operator.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "halo-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "halo-operator.labels" -}}
helm.sh/chart: {{ include "halo-operator.chart" . }}
{{ include "halo-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: halo-operator
{{- end }}

{{- define "halo-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "halo-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "halo-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "halo-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The image reference, defaulting the tag to the chart's appVersion. */}}
{{/* The image reference. With image.digest set it pulls BY DIGEST — the tag is
kept only so a person reading the pod spec can see the version — which is what
a released chart ships, so the version it names is the bytes it runs. Without a
digest it is repository:tag, for local and pre-release installs. */}}
{{- define "halo-operator.image" -}}
{{- $ref := printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- if .Values.image.digest -}}
{{- if not (hasPrefix "sha256:" .Values.image.digest) -}}
{{- fail (printf "image.digest must be a sha256 content digest (sha256:<64 hex>), got %q" .Values.image.digest) -}}
{{- end -}}
{{- printf "%s@%s" $ref .Values.image.digest -}}
{{- else -}}
{{- $ref -}}
{{- end -}}
{{- end }}

{{/* The agent's own ServiceAccount: a separate identity so the write grant on
observations never rides along with the manager's credentials. */}}
{{- define "halo-operator.agentServiceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- printf "%s-agent" (include "halo-operator.fullname" .) }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
