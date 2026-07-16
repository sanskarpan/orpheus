{{/*
Expand the name of the chart.
*/}}
{{- define "orpheus-worker.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "orpheus-worker.fullname" -}}
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

{{- define "orpheus-worker.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orpheus-worker.labels" -}}
helm.sh/chart: {{ include "orpheus-worker.chart" . }}
{{ include "orpheus-worker.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: orpheus
app.kubernetes.io/component: {{ .Values.process }}
{{- end }}

{{- define "orpheus-worker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "orpheus-worker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "orpheus-worker.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s/%s/%s:%s" .Values.image.registry .Values.image.owner .Values.image.repository $tag -}}
{{- end }}

{{- define "orpheus-worker.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "orpheus-worker.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "orpheus-worker.configMapName" -}}
{{- printf "%s-config" (include "orpheus-worker.fullname" .) }}
{{- end }}
