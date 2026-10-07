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

{{/*
The proxy's configuration document. It holds the identity section, so only
the proxy mounts it.

listen.transparent_port must be the port egress-netsetup redirects to. When
it is unset, the proxy does not run a transparent frontend, and the agent's
TCP connections go to a port that nothing listens on.
*/}}
{{- define "keycard-sandbox-user.document" -}}
{{- $root := . -}}
{{- $doc := dict
      "version" 1
      "sandbox" (dict "id" $root.Release.Name)
      "listen" (dict "transparent_port" (int $root.Values.proxy.redirectPort))
      "policy" (dict
        "cluster_cidrs" (default (list) $root.Values.policy.clusterCIDRs)
        "deny_list_exceptions" (default (list) $root.Values.policy.denyListExceptions))
      "endpoints" (default (list) $root.Values.endpoints) -}}
{{- with $root.Values.zoneTrust.configMap -}}
  {{- $_ := set $doc "tls" (dict "upstream_ca_files" (list
        (printf "%s/%s" (trimSuffix "/" $root.Values.zoneTrust.mountPath) $root.Values.zoneTrust.key))) -}}
{{- end -}}
{{- $_ := set $doc "identity" (dict
      "zone_url" (required "zoneURL is required: the zone that decides every token exchange" $root.Values.zoneURL)
      "client_id" (required "clientID is required: the workload-identity credential, from the sandbox_user_access outputs" $root.Values.clientID)
      "workload_token_file" (printf "%s/token" (trimSuffix "/" $root.Values.proxy.tokenPath))
      "impersonate" (dict "user" (include "keycard-sandbox-user.userID" $root))) -}}
{{- toYaml $doc -}}
{{- end }}
