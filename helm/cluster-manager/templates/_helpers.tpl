{{/*
Expand the name of the chart.
*/}}
{{- define "cluster-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "cluster-manager.fullname" -}}
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

{{/*
Chart label value. A label value is at most 63 characters and begins and
ends alphanumeric: the cut of a long version (a branch build's
<version>-dev.<branch>.<date>.<time>.<sha>, or the <version>+<digest>
helm-controller installs) can land on any run of ".", "_" (from "+") and "-",
so the whole run is trimmed.
*/}}
{{- define "cluster-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimAll "-._" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "cluster-manager.labels" -}}
helm.sh/chart: {{ include "cluster-manager.chart" . }}
{{ include "cluster-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "cluster-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cluster-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "cluster-manager.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cluster-manager.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The platform identity contract (global.identity), an empty dict when absent.
*/}}
{{- define "cluster-manager.globalIdentity" -}}
{{- dig "identity" (dict) (.Values.global | default dict) | toJson }}
{{- end }}

{{/*
Existing Secret with the provider credentials of the deprecated keys:
oauth.existingSecret, else the platform's global.identity.existingSecret,
else the chart-rendered one.
*/}}
{{- define "cluster-manager.oauthSecretName" -}}
{{- $g := include "cluster-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.existingSecret | default (dig "existingSecret" "" $g) | default (printf "%s-oauth" (include "cluster-manager.fullname" .)) }}
{{- end }}

{{/*
Whether the chart renders its own OAuth Secret: no
authorizationServer.upstream.client.secretKeyRef, no existing Secret named.
*/}}
{{- define "cluster-manager.oauthRendersSecret" -}}
{{- $g := include "cluster-manager.globalIdentity" . | fromJson -}}
{{- $o := .Values.oauth -}}
{{- if and $o.enabled $o.authorizationServer.enabled (not $o.authorizationServer.upstream.client.secretKeyRef.name) (not $o.existingSecret) (not (dig "existingSecret" "" $g)) }}true{{ end }}
{{- end }}

{{/*
The provider people sign in with: authorizationServer.upstream.type, else the
deprecated oauth.provider, else global.identity.type, else dex.
*/}}
{{- define "cluster-manager.oauthProvider" -}}
{{- $g := include "cluster-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.authorizationServer.upstream.type | default .Values.oauth.provider | default (dig "type" "" $g) | default "dex" }}
{{- end }}

{{/*
The audiences, as JSON, that muster requests at login and the forwarded ID
token then carries for the Kubernetes API server: muster.mcpServer.auth.requiredAudiences,
else global.identity.kubernetes.audiences.
*/}}
{{- define "cluster-manager.requiredAudiences" -}}
{{- $g := include "cluster-manager.globalIdentity" . | fromJson -}}
{{- .Values.muster.mcpServer.auth.requiredAudiences | default (dig "kubernetes" "audiences" list $g) | toJson }}
{{- end }}

{{/*
The OAuth settings the Deployment renders, as JSON. Each comes from the
shared layout (oauth.resourceServer, oauth.authorizationServer), else from
the deprecated key it replaces, else from global.identity:
  issuer          authorizationServer.issuer, baseURL, https://<fullname>.<global.domain>
  provider        authorizationServer.upstream.type, provider, global.identity.type, dex
  upstreamIssuer  authorizationServer.upstream.issuer, dex.issuerURL, global.identity.issuerUrl
  clientID        authorizationServer.upstream.client.id, dex.clientID or google.clientID,
                  global.identity.clientId (dex)
  clientSecret    authorizationServer.upstream.client.secretKeyRef, existingSecret,
                  global.identity.existingSecret, the chart-rendered Secret
  upstreamCA      authorizationServer.upstream.certificateAuthority.secretKeyRef,
                  dex.caSecret, global.identity.ca
  trustedIssuers  resourceServer.trustedIssuers, else one entry for the
                  provider people sign in with (openIDProvider), accepting its
                  ID tokens for the union, in this order and without
                  duplicates, of the deprecated trustedAudiences (else
                  clientID, else global.identity.clientId) and
                  cluster-manager.requiredAudiences: every token muster
                  forwards carries those by construction and the
                  kube-apiserver trusts them, so a portal session's ID token
                  is accepted too
A CA is a mount: {volume, secret or configMap, key, dir}; caFile is dir/key.
*/}}
{{- define "cluster-manager.oauth" -}}
{{- $o := .Values.oauth -}}
{{- $as := $o.authorizationServer -}}
{{- $up := $as.upstream -}}
{{- $rs := $o.resourceServer -}}
{{- $g := include "cluster-manager.globalIdentity" . | fromJson -}}
{{- $domain := dig "domain" "" (.Values.global | default dict) -}}
{{- $derived := "" -}}
{{- if $domain }}{{ $derived = printf "https://%s.%s" (include "cluster-manager.fullname" .) $domain }}{{ end -}}
{{- $r := dict "authorizationServer" $as.enabled "mounts" list -}}
{{- $_ := set $r "issuer" (required "oauth.authorizationServer.issuer is required when oauth.enabled (or set global.domain)" ($as.issuer | default $o.baseURL | default $derived)) -}}
{{- $provider := include "cluster-manager.oauthProvider" . -}}
{{- $_ = set $r "provider" $provider -}}
{{- $upstreamIssuer := "" -}}
{{- $clientID := "" -}}
{{- if eq $provider "dex" -}}
{{- $upstreamIssuer = $up.issuer | default $o.dex.issuerURL | default (dig "issuerUrl" "" $g) -}}
{{- $clientID = $up.client.id | default $o.dex.clientID | default (dig "clientId" "" $g) -}}
{{- else -}}
{{- $upstreamIssuer = "https://accounts.google.com" -}}
{{- $clientID = $up.client.id | default $o.google.clientID -}}
{{- end -}}
{{- $_ = set $r "upstreamIssuer" $upstreamIssuer -}}
{{- $_ = set $r "clientID" $clientID -}}
{{- if $up.client.secretKeyRef.name -}}
{{- $_ = set $r "clientSecret" (dict "name" $up.client.secretKeyRef.name "key" (required "oauth.authorizationServer.upstream.client.secretKeyRef.key is required with its name" $up.client.secretKeyRef.key)) -}}
{{- else -}}
{{- $_ = set $r "clientSecret" (dict "name" (include "cluster-manager.oauthSecretName" .) "key" (printf "%s-client-secret" $provider)) -}}
{{- if and (eq $provider "google") (not $clientID) -}}
{{- $_ = set $r "clientIDSecret" (dict "name" (include "cluster-manager.oauthSecretName" .) "key" "google-client-id") -}}
{{- end -}}
{{- end -}}
{{- if eq $provider "dex" -}}
{{- $ca := dict -}}
{{- if $up.certificateAuthority.secretKeyRef.name -}}
{{- $ca = dict "secret" $up.certificateAuthority.secretKeyRef.name "key" ($up.certificateAuthority.secretKeyRef.key | default "ca.crt") -}}
{{- else if $o.dex.caSecret.name -}}
{{- $ca = dict "secret" $o.dex.caSecret.name "key" ($o.dex.caSecret.key | default "ca.crt") -}}
{{- else if dig "ca" "secretName" "" $g -}}
{{- $ca = dict "secret" (dig "ca" "secretName" "" $g) "key" (dig "ca" "key" "" $g | default $o.dex.caSecret.key | default "ca.crt") -}}
{{- end -}}
{{- if $ca.secret -}}
{{- $_ = set $ca "volume" "idp-ca" -}}
{{- $_ = set $ca "dir" "/etc/cluster-manager/idp-ca" -}}
{{- $_ = set $r "upstreamCA" $ca -}}
{{- $_ = set $r "mounts" (append $r.mounts $ca) -}}
{{- end -}}
{{- end -}}
{{- $issuers := list -}}
{{- range $i, $entry := $rs.trustedIssuers -}}
{{- $out := omit $entry "certificateAuthority" -}}
{{- with $entry.certificateAuthority -}}
{{- $ca := dict "volume" (printf "trusted-issuer-ca-%d" $i) "dir" (printf "/etc/cluster-manager/trusted-issuer-ca/%d" $i) -}}
{{- if .secretKeyRef -}}
{{- $_ := set $ca "secret" (required (printf "oauth.resourceServer.trustedIssuers[%d].certificateAuthority.secretKeyRef.name is required" $i) .secretKeyRef.name) -}}
{{- $_ = set $ca "key" (.secretKeyRef.key | default "ca.crt") -}}
{{- else -}}
{{- $_ := set $ca "configMap" (required (printf "oauth.resourceServer.trustedIssuers[%d].certificateAuthority needs secretKeyRef or configMapKeyRef" $i) (dig "configMapKeyRef" "name" "" .)) -}}
{{- $_ = set $ca "key" (dig "configMapKeyRef" "key" "" . | default "ca.crt") -}}
{{- end -}}
{{- $_ := set $out "caFile" (printf "%s/%s" $ca.dir $ca.key) -}}
{{- $_ = set $r "mounts" (append $r.mounts $ca) -}}
{{- end -}}
{{- $issuers = append $issuers $out -}}
{{- end -}}
{{- if not $issuers -}}
{{- $audiences := list -}}
{{- range concat ($o.trustedAudiences | default (list (dig "clientId" "" $g | default $clientID))) (include "cluster-manager.requiredAudiences" . | fromJsonArray) -}}
{{- if and . (not (has . $audiences)) }}{{ $audiences = append $audiences . }}{{ end -}}
{{- end -}}
{{- if and $audiences $upstreamIssuer -}}
{{- $entry := dict "issuer" $upstreamIssuer "audiences" $audiences "openIDProvider" true -}}
{{- if eq $provider "dex" -}}
{{- /* Dex serves its JWKS at <issuer>/keys and signs ID tokens without a typ header. mcp-oauth does not imply the issuer host for private JWKS addresses, so it is listed. */ -}}
{{- $_ := set $entry "jwksURI" (printf "%s/keys" (trimSuffix "/" $upstreamIssuer)) -}}
{{- $_ = set $entry "acceptedTypHeaders" (list "" "JWT") -}}
{{- $_ = set $entry "allowPrivateIPJWKSHosts" (concat (list (urlParse $upstreamIssuer).hostname) $o.sso.allowPrivateIPJWKSHosts | uniq) -}}
{{- with $r.upstreamCA }}{{ $_ = set $entry "caFile" (printf "%s/%s" .dir .key) }}{{ end -}}
{{- else -}}
{{- $_ := set $entry "jwksURI" "https://www.googleapis.com/oauth2/v3/certs" -}}
{{- $_ = set $entry "acceptedTypHeaders" (list "JWT") -}}
{{- end -}}
{{- $issuers = list $entry -}}
{{- end -}}
{{- end -}}
{{- if not $as.enabled -}}
{{- if not $issuers -}}
{{- fail "oauth.authorizationServer.enabled false accepts only trusted issuer tokens: set oauth.resourceServer.trustedIssuers, or the upstream issuer and client ID it is derived from" -}}
{{- end -}}
{{- if not $rs.authorizationServers -}}
{{- fail "oauth.authorizationServer.enabled false needs oauth.resourceServer.authorizationServers: MCP clients discover the authorization server from the protected resource metadata (RFC 9728), and this server then runs none" -}}
{{- end -}}
{{- end -}}
{{- $_ = set $r "trustedIssuers" $issuers -}}
{{- toJson $r -}}
{{- end }}

{{/*
OTLP trace export env, rendered only with observability.otel.endpoint.
*/}}
{{- define "cluster-manager.otelEnv" -}}
{{- with .Values.observability.otel }}
{{- if .endpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .endpoint | quote }}
- name: OTEL_EXPORTER_OTLP_PROTOCOL
  value: {{ .protocol | quote }}
{{- with .headers }}
- name: OTEL_EXPORTER_OTLP_HEADERS
  value: {{ . | quote }}
{{- end }}
- name: OTEL_TRACES_SAMPLER
  value: {{ .sampler | quote }}
{{- with .samplerArg }}
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ . | quote }}
{{- end }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: NODE_NAME
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
- name: OTEL_RESOURCE_ATTRIBUTES
  value: {{ printf "k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE),k8s.node.name=$(NODE_NAME)%s" (ternary (printf ",%s" .resourceAttributes) "" (ne .resourceAttributes "")) | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
"true" when commit mode fetches the person's GitHub grant per call through
github.tokenExchange (tokenEndpoint set), empty with github off. Fails on a
configuration the server or muster would refuse: github.enabled needs the
exchange, and the exchange needs github.enabled, Dex OAuth with
downstream OAuth (the person's ID token is the subject token), muster
forwarding that token (providerApps needs forwardToken), an audience and a
client secret.
*/}}
{{- define "cluster-manager.githubTokenExchange" -}}
{{- $exchange := .Values.github.tokenExchange }}
{{- if $exchange.tokenEndpoint }}
{{- if not .Values.github.enabled }}
{{- fail "github.tokenExchange.tokenEndpoint is set but github.enabled is false: set github.enabled to fetch the person's GitHub grant per commit, or clear github.tokenExchange" }}
{{- end }}
{{- if not (and .Values.oauth.enabled (eq (include "cluster-manager.oauthProvider" .) "dex") .Values.oauth.downstream.enabled) }}
{{- fail "github.tokenExchange exchanges the person's Dex ID token: set oauth.enabled with the dex provider (oauth.authorizationServer.upstream.type) and oauth.downstream.enabled" }}
{{- end }}
{{- if and .Values.muster.mcpServer.enabled (not .Values.muster.mcpServer.auth.forwardToken) }}
{{- fail "github.tokenExchange lists its audience under the MCPServer's auth.providerApps, which muster accepts only with forwardToken: set muster.mcpServer.auth.forwardToken" }}
{{- end }}
{{- if not $exchange.audience }}
{{- fail "github.tokenExchange.audience is required with github.tokenExchange.tokenEndpoint" }}
{{- end }}
{{- if not (or $exchange.existingSecret $exchange.clientSecret) }}
{{- fail "github.tokenExchange needs github.tokenExchange.existingSecret or github.tokenExchange.clientSecret" }}
{{- end }}
{{- "true" }}
{{- else if or $exchange.audience $exchange.clientID $exchange.clientSecret $exchange.existingSecret $exchange.caSecretName }}
{{- fail "github.tokenExchange.tokenEndpoint is required when any other github.tokenExchange value is set" }}
{{- else if .Values.github.enabled }}
{{- fail "github.enabled needs github.tokenExchange.tokenEndpoint: commit mode fetches the person's GitHub grant per commit from muster's broker (github.tokenExchange)" }}
{{- end }}
{{- end }}

{{/*
The Secret holding the token exchange client: existingSecret, else the one
the chart renders from clientSecret.
*/}}
{{- define "cluster-manager.githubTokenExchangeSecretName" -}}
{{- .Values.github.tokenExchange.existingSecret | default (printf "%s-github-token-exchange" (include "cluster-manager.fullname" .)) }}
{{- end }}
