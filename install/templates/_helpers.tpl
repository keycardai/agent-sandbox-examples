{{/*
Labels every object in the release carries.
*/}}
{{- define "keycard-sandbox.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{/*
The sandbox namespace, quoted. Rendering fails unless it is a DNS label, as
Kubernetes requires of a namespace name.
*/}}
{{- define "keycard-sandbox.namespace" -}}
{{- $namespace := toString .Values.namespace -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$" $namespace) -}}
{{- fail (printf "namespace %q must be a DNS label" $namespace) -}}
{{- end -}}
{{- $namespace | quote -}}
{{- end }}
