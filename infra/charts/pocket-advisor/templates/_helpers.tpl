{{/*
Common labels. app.kubernetes.io/part-of is load-bearing: the VMServiceScrape
selector matches on it, so a workload missing this label is silently not
scraped (ingestion-design.md §9.2).
*/}}
{{- define "pa.labels" -}}
app.kubernetes.io/part-of: rag-ingestion-engine
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "pa.metricsAnnotations" -}}
prometheus.io/scrape: "true"
prometheus.io/port: "{{ .port }}"
prometheus.io/path: "/metrics"
{{- end }}

{{/*
Infrastructure endpoints, identical for every workload.
*/}}
{{- define "pa.natsEnv" -}}
- name: NATS_URL
  value: "nats://{{ .Release.Name }}-nats:{{ .Values.nats.clientPort }}"
{{- end }}

{{- define "pa.postgresEnv" -}}
- name: POSTGRES_DSN
  valueFrom:
    secretKeyRef:
      name: {{ .Release.Name }}-credentials
      key: postgres-dsn
{{- end }}

{{/*
Object store (RustFS) with the WORKER identity: read anywhere, write only
under extracted/. raw/ is the uploader's alone, and that is enforced by
policy rather than by convention (§5.1).

Env var names stay MINIO_* deliberately: this is our own Go code's
contract (internal/config), independent of which S3-compatible backend
actually runs behind it — renaming these would touch every worker's
config-loading code for no functional benefit, the same reasoning as
leaving the Go module path as .../v3 after the directory move.
*/}}
{{- define "pa.minioWorkerEnv" -}}
- name: MINIO_ENDPOINT
  value: "{{ .Release.Name }}-rustfs:{{ .Values.rustfs.apiPort }}"
- name: MINIO_BUCKET
  value: {{ .Values.rustfs.bucket | quote }}
- name: MINIO_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Release.Name }}-credentials
      key: rustfs-worker-access-key
- name: MINIO_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Release.Name }}-credentials
      key: rustfs-worker-secret-key
{{- end }}

{{/*
Object store (RustFS) with the UPLOADER identity: the only credentials that
may write raw/ or delete anything. See pa.minioWorkerEnv above for why the
env var names stay MINIO_*.
*/}}
{{- define "pa.minioUploaderEnv" -}}
- name: MINIO_ENDPOINT
  value: "{{ .Release.Name }}-rustfs:{{ .Values.rustfs.apiPort }}"
- name: MINIO_BUCKET
  value: {{ .Values.rustfs.bucket | quote }}
- name: MINIO_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Release.Name }}-credentials
      key: rustfs-uploader-access-key
- name: MINIO_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Release.Name }}-credentials
      key: rustfs-uploader-secret-key
{{- end }}

{{- define "pa.embeddingEnv" -}}
- name: EMBEDDING_ENDPOINT
  value: {{ .Values.embedding.endpoint | quote }}
- name: EMBEDDING_MODEL
  value: {{ .Values.embedding.model | quote }}
{{- if .Values.embedding.apiKeySecret }}
- name: EMBEDDING_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.embedding.apiKeySecret }}
      key: {{ .Values.embedding.apiKeySecretKey | default "api-key" }}
{{- end }}
{{- end }}

{{- define "pa.commonEnv" -}}
- name: LOG_LEVEL
  value: {{ .Values.global.logLevel | quote }}
{{- end }}
