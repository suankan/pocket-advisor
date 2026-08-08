{{- define "pocket-advisor-app.name" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pocket-advisor-app.labels" -}}
app.kubernetes.io/name: pocket-advisor
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: authenticated-mcp
{{- end -}}
