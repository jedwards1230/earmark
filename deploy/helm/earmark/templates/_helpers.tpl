{{/*
Expand the name of the chart.
*/}}
{{- define "earmark.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "earmark.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Image tag — defaults to Chart.AppVersion.
*/}}
{{- define "earmark.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Common labels (all resources).
*/}}
{{- define "earmark.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "earmark.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: earmark-stack
{{- end }}

{{/*
Selector labels shared by BOTH deployments — used by the Service to target mcp pods.
The Service only routes to mcp pods, so selector must include the component label.
*/}}
{{- define "earmark.selectorLabels" -}}
app.kubernetes.io/name: {{ include "earmark.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
MCP component: name, selector labels.
*/}}
{{- define "earmark.mcp.name" -}}
{{- printf "%s-mcp" (include "earmark.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "earmark.mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "earmark.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: mcp-server
{{- end }}

{{/*
Ingest component: name, selector labels.
*/}}
{{- define "earmark.ingest.name" -}}
{{- printf "%s-ingest" (include "earmark.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "earmark.ingest.selectorLabels" -}}
app.kubernetes.io/name: {{ include "earmark.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: ingest
{{- end }}

{{/*
Config checksum annotation — roll pods when ConfigMap/env config changes.
Pass the entire .Values.config as the checksum source.
*/}}
{{- define "earmark.configChecksum" -}}
checksum/config: {{ .Values.config | toJson | sha256sum }}
{{- end }}

{{/*
Shared container env block (database + app config) — avoids duplication across
the two Deployments.
*/}}
{{- define "earmark.commonEnv" -}}
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
{{- with .Values.config.libraryCollections }}
- name: LIBRARY_COLLECTIONS
  value: {{ toJson . | quote }}
{{- end }}
{{- with .Values.config.asrServers }}
- name: ASR_SERVERS
  value: {{ toJson . | quote }}
{{- end }}
{{- /*
  AI endpoint registry (CONTRACT §2.14). Pass-through JSON, same precedent as
  ASR_SERVERS above. When aiEndpoints is empty the env vars are omitted and the
  app synthesizes a `_legacy` embeddings endpoint from EMBEDDINGS_BASE_URL/
  EMBEDDINGS_MODEL — so the legacy path stays intact. When aiEndpoints is set,
  aiRoles.embeddings is required (the app fails closed without it); guard at
  render time, mirroring the absURL/absToken guard below.
*/}}
{{- if and .Values.config.aiEndpoints (not (and .Values.config.aiRoles .Values.config.aiRoles.embeddings)) }}
{{- fail "config.aiEndpoints is set but config.aiRoles.embeddings is empty. AI_ROLES.embeddings is required when AI_ENDPOINTS is set (CONTRACT §2.14): set config.aiRoles.embeddings to the id of an embeddings endpoint, or clear config.aiEndpoints to use the legacy EMBEDDINGS_* path." }}
{{- end }}
{{- with .Values.config.aiEndpoints }}
- name: AI_ENDPOINTS
  value: {{ toJson . | quote }}
{{- end }}
{{- with .Values.config.aiRoles }}
- name: AI_ROLES
  value: {{ toJson . | quote }}
{{- end }}
{{- /*
  Eval-layer chat endpoint (CONTRACT §2.15). Standalone EVAL_CHAT_* env vars
  consumed by internal/eval when a chat endpoint (e.g. vLLM) exists. Each is
  emitted only when set, so an unset evalChat leaves the eval layer unconfigured
  (eval is on-demand and degrades gracefully). EVAL_CHAT_API_KEY is a plain value
  passthrough for now — move it to a secretKeyRef if the endpoint needs a real
  credential.
*/}}
{{- with .Values.config.evalChat.baseURL }}
- name: EVAL_CHAT_BASE_URL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.config.evalChat.model }}
- name: EVAL_CHAT_MODEL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.config.evalChat.apiKey }}
- name: EVAL_CHAT_API_KEY
  value: {{ . | quote }}
{{- end }}
{{- /*
  In-pipeline eval (CONTRACT §2.15): when true, the embed worker runs the eval
  judge on each transcript's chunks BEFORE embedding. Emitted only when enabled;
  unset → the app default (false; eval stays on-demand). Cost is bounded by the
  batch coordinator (run_limit) — leave off unless running the batched pipeline.
*/}}
{{- if .Values.config.evalInPipeline }}
- name: EVAL_IN_PIPELINE
  value: "true"
{{- end }}
- name: METADATA_PROVIDER
  value: {{ .Values.config.metadataProvider | quote }}
{{- /*
  ABS atomicity guard: ABS_TOKEN is required whenever ABS_URL is set
  (CONTRACT §2.4). Without this, a URL-but-no-token config silently degrades
  to the path provider at runtime (the Go factory falls back rather than
  crashing) — confusing to debug. Fail fast at render time instead.
*/}}
{{- if and .Values.config.absURL (not (and .Values.secrets.enabled .Values.secrets.absToken.itemPath)) }}
{{- fail "config.absURL is set but secrets.absToken.itemPath is empty (or secrets.enabled is false). ABS_TOKEN is required when ABS_URL is set (CONTRACT §2.4): set secrets.absToken.itemPath, or clear config.absURL to use the path provider." }}
{{- end }}
{{- with .Values.config.absURL }}
- name: ABS_URL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.config.absLibraryID }}
- name: ABS_LIBRARY_ID
  value: {{ . | quote }}
{{- end }}
{{- if and .Values.secrets.enabled .Values.secrets.absToken.itemPath }}
- name: ABS_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.absToken.name | quote }}
      key: {{ .Values.secrets.absToken.key | quote }}
{{- end }}
{{- end }}

{{/*
Shared container securityContext.
*/}}
{{- define "earmark.containerSecurityContext" -}}
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
{{- define "earmark.podSecurityContext" -}}
fsGroup: 100
{{- end }}

{{/*
Shared volumeMounts for both containers.
*/}}
{{- define "earmark.volumeMounts" -}}
- name: books
  mountPath: {{ .Values.config.booksDir }}
  readOnly: true
- name: tmp
  mountPath: /tmp
{{- end }}

{{/*
Shared volumes spec for both pods.
*/}}
{{- define "earmark.volumes" -}}
- name: books
  persistentVolumeClaim:
    claimName: {{ required "booksPvcName is required" .Values.booksPvcName | quote }}
    readOnly: true
- name: tmp
  emptyDir: {}
{{- end }}
