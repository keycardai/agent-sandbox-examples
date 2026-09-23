{{/*
The labels on every object in the release.
*/}}
{{- define "keycard-sandbox-user.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
keycard.ai/user-id: {{ .Values.userID | quote }}
{{- end }}

{{/*
Annotations that name the zone entities this sandbox depends on, from the
zone-setup run that enrolled the user.
*/}}
{{- define "keycard-sandbox-user.annotations" -}}
keycard.ai/user-email: {{ .Values.userEmail | quote }}
keycard.ai/provider-id: {{ .Values.providerID | quote }}
keycard.ai/application-id: {{ .Values.applicationID | quote }}
keycard.ai/application-identifier: {{ .Values.applicationIdentifier | quote }}
keycard.ai/client-id: {{ .Values.clientID | quote }}
{{- end }}

{{/*
The user's IAM public id. Rendering fails when serviceAccount is set to any
subject other than system:serviceaccount:<namespace>:<userID>. A sandbox with
another subject would fail every exchange with access_denied.
*/}}
{{- define "keycard-sandbox-user.userID" -}}
{{- $user := required "userID is required: the IAM user public id, from the sandbox_user_access outputs" .Values.userID -}}
{{- $subject := printf "system:serviceaccount:%s:%s" .Release.Namespace $user -}}
{{- if and .Values.serviceAccount (ne .Values.serviceAccount $subject) -}}
{{- fail (printf "serviceAccount is %q, but this sandbox binds %q" .Values.serviceAccount $subject) -}}
{{- end -}}
{{- $user -}}
{{- end }}

{{- define "keycard-sandbox-user.proxyConfigMap" -}}
{{- printf "%s-proxy-config" .Release.Name -}}
{{- end }}

{{- define "keycard-sandbox-user.agentConfigMap" -}}
{{- printf "%s-agent-config" .Release.Name -}}
{{- end }}

{{/*
The configuration document. Pass `identity` true for the proxy's copy, and
false for the agent's copy, which leaves out the identity section.
*/}}
{{- define "keycard-sandbox-user.document" -}}
{{- $root := .root -}}
{{- $doc := dict
      "version" 1
      "sandbox" (dict "id" $root.Release.Name)
      "policy" (dict
        "cluster_cidrs" (default (list) $root.Values.policy.clusterCIDRs)
        "deny_list_exceptions" (default (list) $root.Values.policy.denyListExceptions))
      "endpoints" (default (list) $root.Values.endpoints) -}}
{{- if .identity -}}
  {{- $_ := set $doc "identity" (dict
        "zone_url" (required "zoneURL is required: the zone that decides every token exchange" $root.Values.zoneURL)
        "client_id" (required "clientID is required: the workload-identity credential, from the sandbox_user_access outputs" $root.Values.clientID)
        "workload_token_file" (printf "%s/token" (trimSuffix "/" $root.Values.proxy.tokenPath))
        "impersonate" (dict "user" (include "keycard-sandbox-user.userID" $root))) -}}
{{- end -}}
{{- toYaml $doc -}}
{{- end }}
