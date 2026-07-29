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
