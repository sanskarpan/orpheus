{{/*
Expand the name of the chart.
*/}}
{{- define "orpheus-api.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this.
*/}}
{{- define "orpheus-api.fullname" -}}
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
Chart name and version as used by the chart label.
*/}}
{{- define "orpheus-api.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "orpheus-api.labels" -}}
helm.sh/chart: {{ include "orpheus-api.chart" . }}
{{ include "orpheus-api.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: orpheus
{{- end }}

{{/*
Selector labels
*/}}
{{- define "orpheus-api.selectorLabels" -}}
app.kubernetes.io/name: {{ include "orpheus-api.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Fully qualified image reference.
*/}}
{{- define "orpheus-api.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s/%s/%s:%s" .Values.image.registry .Values.image.owner .Values.image.repository $tag -}}
{{- end }}

{{/*
Name of the service account to use.
*/}}
{{- define "orpheus-api.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "orpheus-api.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the ConfigMap holding non-secret configuration.
*/}}
{{- define "orpheus-api.configMapName" -}}
{{- printf "%s-config" (include "orpheus-api.fullname" .) }}
{{- end }}
