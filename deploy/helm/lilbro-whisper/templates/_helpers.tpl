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
Image tag — defaults to Chart.AppVersion.
*/}}
{{- define "lilbro-whisper.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Common labels (all resources).
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
Selector labels shared by BOTH deployments — used by the Service to target mcp pods.
The Service only routes to mcp pods, so selector must include the component label.
*/}}
{{- define "lilbro-whisper.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lilbro-whisper.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
MCP component: name, selector labels.
*/}}
{{- define "lilbro-whisper.mcp.name" -}}
{{- printf "%s-mcp" (include "lilbro-whisper.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "lilbro-whisper.mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lilbro-whisper.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: mcp-server
{{- end }}

{{/*
Ingest component: name, selector labels.
*/}}
{{- define "lilbro-whisper.ingest.name" -}}
{{- printf "%s-ingest" (include "lilbro-whisper.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "lilbro-whisper.ingest.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lilbro-whisper.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: ingest
{{- end }}

{{/*
Config checksum annotation — roll pods when ConfigMap/env config changes.
Pass the entire .Values.config as the checksum source.
*/}}
{{- define "lilbro-whisper.configChecksum" -}}
checksum/config: {{ .Values.config | toJson | sha256sum }}
{{- end }}

{{/*
Shared container env block (database + app config) — avoids duplication across
the two Deployments.
*/}}
{{- define "lilbro-whisper.commonEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ required "databaseSecret.name is required" .Values.databaseSecret.name | quote }}
      key: {{ required "databaseSecret.key is required" .Values.databaseSecret.key | quote }}
- name: BOOKS_DIR
  value: {{ .Values.config.booksDir | quote }}
- name: EMBEDDINGS_BASE_URL
  value: {{ required "config.embeddingsBaseURL is required" .Values.config.embeddingsBaseURL | quote }}
- name: EMBEDDINGS_MODEL
  value: {{ .Values.config.embeddingsModel | quote }}
- name: STALE_JOB_TIMEOUT
  value: {{ .Values.config.staleJobTimeout | quote }}
- name: CHUNK_SIZE
  value: {{ .Values.config.chunkSize | quote }}
{{- end }}

{{/*
Shared container securityContext.
*/}}
{{- define "lilbro-whisper.containerSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
seccompProfile:
  type: RuntimeDefault
capabilities:
  drop:
    - ALL
{{- end }}

{{/*
Shared pod securityContext.
*/}}
{{- define "lilbro-whisper.podSecurityContext" -}}
fsGroup: 100
{{- end }}

{{/*
Shared volumeMounts for both containers.
*/}}
{{- define "lilbro-whisper.volumeMounts" -}}
- name: books
  mountPath: {{ .Values.config.booksDir }}
  readOnly: true
- name: tmp
  mountPath: /tmp
{{- end }}

{{/*
Shared volumes spec for both pods.
*/}}
{{- define "lilbro-whisper.volumes" -}}
- name: books
  persistentVolumeClaim:
    claimName: {{ required "booksPvcName is required" .Values.booksPvcName | quote }}
    readOnly: true
- name: tmp
  emptyDir: {}
{{- end }}
