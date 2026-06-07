{{/*
Expand the name of the chart.
*/}}
{{- define "lilbro-whisper.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "lilbro-whisper.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "lilbro-whisper.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "lilbro-whisper.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: lilbro-whisper-stack
{{- end }}

{{/*
Selector labels
*/}}
{{- define "lilbro-whisper.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lilbro-whisper.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Image tag — defaults to Chart.appVersion.
*/}}
{{- define "lilbro-whisper.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}
