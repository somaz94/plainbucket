{{/*
Expand the name of the chart.
*/}}
{{- define "plainbucket.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "plainbucket.fullname" -}}
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
{{- define "plainbucket.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "plainbucket.labels" -}}
helm.sh/chart: {{ include "plainbucket.chart" . }}
{{ include "plainbucket.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "plainbucket.selectorLabels" -}}
app.kubernetes.io/name: {{ include "plainbucket.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "plainbucket.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "plainbucket.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Secret holding the full-access key: auth.existingSecret, or the one this chart creates
from auth.accessKey / auth.secretKey. Empty when only auth.credentialsSecret gives keys.
*/}}
{{- define "plainbucket.authSecret" -}}
{{- $a := .Values.auth -}}
{{- if $a.existingSecret -}}
{{- $a.existingSecret -}}
{{- else if or $a.accessKey $a.secretKey -}}
{{- if not (and $a.accessKey $a.secretKey) }}
{{- fail "auth.accessKey and auth.secretKey must be set together" }}
{{- end -}}
{{- printf "%s-auth" (include "plainbucket.fullname" .) -}}
{{- else if not $a.credentialsSecret -}}
{{- fail "no access key: set auth.existingSecret, auth.accessKey and auth.secretKey, or auth.credentialsSecret" }}
{{- end -}}
{{- end }}

{{/*
Name of the data claim (chart-created or existing).
*/}}
{{- define "plainbucket.dataClaimName" -}}
{{- .Values.persistence.existingClaim | default (printf "%s-data" (include "plainbucket.fullname" .)) -}}
{{- end }}

{{/*
"true" when the server needs a config file: the settings that only exist in YAML.
*/}}
{{- define "plainbucket.configFile" -}}
{{- if or .Values.config.buckets .Values.config.rateLimit .Values.config.anonymousRateLimit }}true{{ end }}
{{- end }}
