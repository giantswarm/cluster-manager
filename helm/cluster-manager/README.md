# cluster-manager

CIMD is enabled by default. DCR is deprecated by MCP 2026-07-28 and remains a
compatibility fallback; see the [OAuth registration guide](../../docs/oauth-client-registration.md).

The Agent Platform's MCP-only cluster write surface — the installation's clusters and their GPU node pools, listed and written as the person, exposed to agents and the portal through muster

The chart deploys one Deployment that serves the MCP streamable-HTTP endpoint
under `/mcp` — cluster-manager's only surface. Its tools (`get_info`,
`list_clusters`, `list_releases`, `create_cluster`, `delete_cluster`,
`list_node_pools`, `create_node_pool`, `delete_node_pool`,
`enable_model_serving`, `disable_model_serving`, `remove_model_cache`) read the
installation's Cluster API objects, Flux HelmReleases and Giant Swarm Releases.

With `oauth.enabled` the server is an mcp-oauth resource server: muster
forwards the session's IdP token (`muster.mcpServer.auth.forwardToken`), the
server validates it against its trusted audiences and, with
`oauth.downstream.enabled` (the default), presents it to the Kubernetes API —
every call runs as the person, and the ServiceAccount gets no RBAC. Without
OAuth the chart renders a read-only ClusterRole for the ServiceAccount instead.

A component of the [`giantswarm/agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart, which sets `installation.name`, `oauth.*` and `muster.mcpServer.*`
from the platform identity contract (`global.identity`, `global.domain`).

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| global | object | `{}` | Platform-wide values a parent chart (the `agent-platform` meta chart) shares with every component; Helm forwards them to this chart. `oauth.*` reads the identity contract as its defaults: `global.identity.issuerUrl`, `global.identity.clientId`, `global.identity.existingSecret`, `global.identity.ca.secretName` / `.key`, and `global.domain` for the OAuth base URL. Empty here; a chart installed on its own sets `oauth.*` directly. |
| replicaCount | int | `1` | Number of replicas. The server holds no state; more than one is fine. |
| revisionHistoryLimit | int | `3` | Old ReplicaSets the Deployment keeps. Kubernetes defaults to 10; fewer keeps stale pod templates out of the PolicyReport. |
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/cluster-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified app name. |
| installation.name | string | `""` | Name of the installation. `list_clusters` reports the Cluster of that name as the installation's own cluster (its management cluster); empty marks none. |
| createdClusters.oidc.clientID | string | `""` | Audience the cluster's apiserver accepts in the person's id_token; empty is the first of `muster.mcpServer.auth.requiredAudiences`, the cross-client audience muster requests at login. Both empty: created clusters trust no identity provider. |
| createdClusters.oidc.usernameClaim | string | `"email"` | id_token claim the cluster's apiserver takes as the username. |
| createdClusters.oidc.groupsClaim | string | `"groups"` | id_token claim the cluster's apiserver takes as the groups. |
| modelManager.namespace | string | `"agent-platform"` | Namespace model-manager runs in on the installation. `create_node_pool` registers each serving cluster's kserve backend there by writing its ConfigMap of model-manager's runtime-registration contract (`model-backend-kserve` for the installation's own cluster, `model-backend-kserve-<cluster>` for a workload cluster); `delete_node_pool` of the cluster's last pool removes that cluster's; `enable_model_serving` and `disable_model_serving` register and remove it too. |
| serving.namespace | string | `"model-serving"` | Namespace on a serving cluster where the LLMInferenceServices go, named as `target.servingNamespace` in the registered `kserve` backend. |
| serving.cacheClaim | string | `"hf-cache"` | Base name of the model cache `PersistentVolumeClaim`s in the serving namespace (the connectivity chart's default `modelServing.cache.pvc.name`). A claim outlives the pool and is one volume bound in one zone, so the cache is one claim per zone: `create_node_pool` with `zones` composes the zone's claim into the pool's slice — the claim Bound there, else `<cacheClaim>-<zone>` — and pins the pool's nodes to the zone (the gpu-node-pool chart's `pool.zones`); `list_clusters` names every claim with its zone under serving readiness. |
| serving.sliceChartVersion | string | `""` | Exact `agent-platform` chart version the `<cluster>-agent-platform` slice release pins. Empty follows the chart from the version the installation's own platform release runs (at least 4.85.0, the first chart that names a targeted release's children per target) to the next major; set it only for a lab running an unreleased chart. |
| serving.certificateIssuer | string | `"letsencrypt-giantswarm"` | cert-manager `ClusterIssuer` the `<cluster>-agent-platform` slice release asks for the certificate of `models.<domain>` when the platform's wildcard is not usable: on a workload cluster (another domain) and on the installation's own cluster when the platform's release names no `gatewayApi.gateway.tls.secretName` (TLS terminated at the fleet's edge Gateway, its wildcard in another namespace). `letsencrypt-giantswarm` is the issuer every Giant Swarm cluster runs. Empty composes none — the connectivity chart then refuses a models Gateway without a certificate. |
| gpuOperator.dcgmExporter | bool | `false` | Run NVIDIA's DCGM exporter on the GPU pools' nodes through the composed `<cluster>-gpu-operator` release (its `dcgmExporter.enabled`), for an installation whose observability scrapes it (a ServiceMonitor on the `nvidia-dcgm-exporter` Service in `kube-system`). Off by default: nothing scrapes the exporter out of the box, and on a fresh pool node it is an image pull and a pod initialising DCGM on the GPU while the device plugin brings `nvidia.com/gpu` up. |
| flux.tenantServiceAccount | string | `"automation"` | ServiceAccount in every org namespace the installation's Flux runs a composed release under when the release's objects live in that namespace: the pool release (its Cluster API objects) and the `<cluster>-agent-platform` slice release (its child HelmReleases). Giant Swarm installations enforce the `flux-multi-tenancy` policy on every HelmRelease in `org-<org>` — a `serviceAccountName` or a kubeconfig on each —, and `automation` is the org's tenant ServiceAccount there. A release installing into a cluster (the `<cluster>-gpu-operator` release, the slice's children) is delivered through the cluster's `<cluster>-kubeconfig` Secret instead, the installation's own cluster included. Empty renders none, for a lab without the policy. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint (the server's only surface). |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make cluster-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint requires a bearer token, and every call carries the caller's identity. On the Agent Platform muster forwards the session's IdP ID token to this server (MCPServer `auth.forwardToken`, rendered below) and the portal's calls arrive the same way through muster; both are accepted from the provider people sign in with (`resourceServer.trustedIssuers`). Off: anonymous, acting as the ServiceAccount, only for a server nothing but a trusted proxy can reach. |
| oauth.resourceServer.resource | string | `""` | The main protected resource, by its full URL (`OAUTH_RESOURCE`). Empty uses the issuer. |
| oauth.resourceServer.additionalResources | list | `[]` | Further protected resources on the same origin, each by its full URL (`OAUTH_ADDITIONAL_RESOURCES`). The MCP endpoint is always a protected resource. |
| oauth.resourceServer.authorizationServers | list | `[]` | Issuer identifiers the resource metadata advertises (`OAUTH_AUTHORIZATION_SERVERS`, RFC 9728 `authorization_servers`): this server's issuer or a trusted issuer. Empty advertises this server's issuer; required with `authorizationServer.enabled` false, since MCP clients find the authorization server here. |
| oauth.resourceServer.trustedIssuers | list | `[]` | The issuers whose JWTs are accepted (`OAUTH_TRUSTED_ISSUERS`). A trust anchor: set it only from the release's own values or a GitOps-managed ConfigMap that only installation administrators write. Each entry: `issuer`, `jwksURI` and `audiences` (required), `subjectClaim` (`sub` or `email`), `acceptedTypHeaders`, `allowedClaims`, `allowPrivateIPJWKSHosts`, `openIDProvider` (true for the provider people sign in with, whose bearers are their ID tokens), and `certificateAuthority` with a `secretKeyRef` or `configMapKeyRef` (`name`, `key`). Empty trusts the provider people sign in with (`authorizationServer.upstream`) as `openIDProvider`: its ID tokens are accepted as bearer tokens, a deviation from MCP's audience rule until per-backend tokens (ID-JAG, giantswarm/giantswarm#39102). They are accepted for the client people sign in with (`authorizationServer.upstream.client.id`, or the deprecated `oauth.trustedAudiences`) and `muster.mcpServer.auth.requiredAudiences`, in that order and without duplicates. |
| oauth.resourceServer.requireResourceAccessTokens | bool | `false` | Accept only tokens whose audience names this server's resource (`OAUTH_REQUIRE_RESOURCE_ACCESS_TOKENS`), as MCP requires. Off while the platform forwards ID tokens (giantswarm/giantswarm#39102). |
| oauth.authorizationServer.enabled | bool | `true` | Run the authorization server. Off makes cluster-manager a resource server only (`OAUTH_ENABLE_AUTHORIZATION_SERVER=false`). |
| oauth.authorizationServer.issuer | string | `""` | Public base URL of this server, the issuer of its tokens and metadata (`OAUTH_ISSUER`; https, or http on loopback). Empty uses the deprecated `oauth.baseURL`, else `https://<fullname>.<global.domain>`. |
| oauth.authorizationServer.upstream.type | string | `""` | The OpenID Provider people sign in with: `dex` or `google` (`OAUTH_PROVIDER`). Empty uses the deprecated `oauth.provider`, else `dex`. |
| oauth.authorizationServer.upstream.issuer | string | `""` | Dex issuer URL. Empty uses the deprecated `oauth.dex.issuerURL`, else `global.identity.issuerUrl`. |
| oauth.authorizationServer.upstream.client.id | string | `""` | OAuth client ID at the provider. Empty uses the deprecated `oauth.dex.clientID` or `oauth.google.clientID`, else `global.identity.clientId` (dex). |
| oauth.authorizationServer.upstream.client.secretKeyRef | object | `{"key":"client-secret","name":""}` | Secret key with the client secret, mounted as a file. Empty name uses the deprecated `oauth.existingSecret`, else `global.identity.existingSecret`, else a Secret from the deprecated inline client secret. |
| oauth.authorizationServer.upstream.certificateAuthority.secretKeyRef | object | `{"key":"ca.crt","name":""}` | Secret key with the CA of a Dex that serves a private certificate (`OAUTH_DEX_CA_FILE`). Empty name uses the deprecated `oauth.dex.caSecret`, else `global.identity.ca`. |
| oauth.authorizationServer.dynamicClientRegistration.enabled | bool | `true` | Serve RFC 7591 dynamic client registration (`OAUTH_ENABLE_DYNAMIC_CLIENT_REGISTRATION`). MCP 2026-07-28 deprecates it in favour of client ID metadata documents; it stays on for clients that still register this way. |
| oauth.authorizationServer.dynamicClientRegistration.initialAccessToken.secretKeyRef | object | `{"key":"token","name":""}` | Secret key with the RFC 7591 initial access token a registration must present (mcp-oauth reads it from `OAUTH_INITIAL_ACCESS_TOKEN_FILE`). |
| oauth.authorizationServer.clientIDMetadataDocuments.enabled | bool | `true` | Accept URL client IDs (client ID metadata documents) (`OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS`). |
| oauth.authorizationServer.storage.type | string | `"memory"` | Token storage (`OAUTH_STORAGE_BACKEND`). Memory loses every session on restart and needs one replica. |
| oauth.baseURL | string | `""` | Deprecated: use `authorizationServer.issuer`. |
| oauth.provider | string | `""` | Deprecated: use `authorizationServer.upstream.type`. |
| oauth.dex.issuerURL | string | `""` | Deprecated: use `authorizationServer.upstream.issuer`. |
| oauth.dex.clientID | string | `""` | Deprecated: use `authorizationServer.upstream.client.id`. |
| oauth.dex.clientSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. |
| oauth.dex.allowPrivateURLs | bool | `false` | Deprecated, no effect, not passed to the server: the Dex client reaches private addresses and follows HTTP_PROXY, HTTPS_PROXY and NO_PROXY without it. Remove it from your values; the key goes away in a later release. |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Deprecated: use `authorizationServer.upstream.certificateAuthority`. |
| oauth.google.clientID | string | `""` | Deprecated: use `authorizationServer.upstream.client.id`. Empty reads the Secret key `google-client-id`. |
| oauth.google.clientSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. |
| oauth.existingSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. Existing Secret with `dex-client-secret` (dex) or `google-client-secret` (+ optional `google-client-id`) (google). |
| oauth.trustedAudiences | list | `[]` | Deprecated: list the provider in `resourceServer.trustedIssuers` with these `audiences`. The OAuth client IDs whose ID tokens of the provider people sign in with are accepted. |
| oauth.sso.allowPrivateIPs | bool | `false` | Deprecated, no effect, not passed to the server: the issuer host of the configured provider may resolve to a private address for its JWKS without it; list any other JWKS host in allowPrivateIPJWKSHosts. Remove it from your values; the key goes away in a later release. |
| oauth.sso.allowPrivateIPJWKSHosts | list | `[]` | Deprecated: list the provider in `resourceServer.trustedIssuers` with these `allowPrivateIPJWKSHosts`. Hosts, other than the issuer's own, whose JWKS may resolve to an RFC 1918 or RFC 4193 address. |
| oauth.allowPublicClientRegistration | bool | `false` | Deprecated, removed in a later release: accept dynamic client registration without an initial access token, public clients included (labs only). MCP clients register with client ID metadata documents. |
| oauth.downstream.enabled | bool | `true` | Call the Kubernetes API as the caller: everything a request does presents the caller's IdP token, so the caller's RBAC governs — the apiserver must trust the IdP and the token's audience (a Dex install lists that audience in `muster.mcpServer.auth.requiredAudiences`; a Google install's client id is the apiserver's `--oidc-client-id`). This is cluster-manager's rule (every write as the person), so it is on by default and takes effect with `oauth.enabled`. The ServiceAccount then holds no permissions: the chart renders no RBAC for it (`rbac.create` is moot). |
| github.enabled | bool | `false` | Offer commit mode (`mode: commit` on `create_cluster`, `delete_cluster`, `create_node_pool` and `delete_node_pool`: the pull request opened as the person in the repository that owns the cluster). Needs `oauth.enabled` and `tokenExchange.tokenEndpoint`: muster keeps forwarding the person's ID token as the bearer (`muster.mcpServer.auth.forwardToken`) and the server fetches the person's GitHub App user token for each commit (RFC 8693). Off: commit mode is refused. |
| github.apiURL | string | `"https://api.github.com"` | GitHub's REST API, for `GET /user` and the pull request. |
| github.tokenExchange.tokenEndpoint | string | `""` | RFC 8693 token endpoint (muster's broker) that releases the person's GitHub App user token for a commit. Set: only a commit asks for GitHub, and one without the person's grant answers `grant_required` naming `audience`. Needs `github.enabled`, `oauth.enabled` with Dex, `oauth.downstream.enabled` and `muster.mcpServer.auth.forwardToken`; required with `github.enabled`. |
| github.tokenExchange.audience | string | `""` | RFC 8693 audience: the App whose grant the broker releases. Also the App named in `grant_required` answers and listed under the MCPServer's `auth.providerApps`, so muster answers with that App's sign-in link. The App needs contents and pull-request write on every repository a commit targets. Required with `tokenEndpoint`. |
| github.tokenExchange.clientID | string | `""` | Confidential broker client ID. Empty: read from the Secret's `client-id` key. |
| github.tokenExchange.clientSecret | string | `""` | Confidential broker client secret; the chart renders it into a Secret. Prefer `existingSecret`, which wins when both are set. |
| github.tokenExchange.existingSecret | string | `""` | Existing Secret in the release namespace with `client-secret` (and `client-id` when `clientID` is empty). |
| github.tokenExchange.caSecretName | string | `""` | Optional existing Secret with `ca.crt` for the broker's private CA. |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"cluster-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.displayName | string | `"Cluster Manager"` | Human-friendly name the Dev Portal shows for this server, rendered as the `ui.giantswarm.io/display-name` annotation. Empty renders none. |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Clusters and GPU node pools of this installation (list, and the node-pool writes as the person) for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| muster.mcpServer.auth | object | `{"forwardToken":true,"requiredAudiences":[]}` | How muster authenticates to this server; rendered only with `oauth.enabled`. `forwardToken` makes muster forward the session's IdP id_token byte-identical (the SSO path this chart trusts through its trusted audiences). `requiredAudiences` are extra audiences that token must carry — the Dex cross-client audience the kube-apiserver trusts (`dex-k8s-authenticator` on Giant Swarm clusters; agentlab's is `kubernetes`) so `oauth.downstream` works; muster requests them at login, so users re-login after a change. They are trusted as bearer audiences by construction, since the server's trusted audiences are `oauth.trustedAudiences` plus this list. A Google IdP has no cross-client audiences: leave the list empty. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the read-only ClusterRole/ClusterRoleBinding (Cluster API clusters, machine pools, control planes and infrastructure, Flux HelmReleases, Giant Swarm Releases) — the ServiceAccount's own permissions for a release without OAuth. Ignored with `oauth.enabled` and `oauth.downstream.enabled`: the ServiceAccount then gets no RBAC at all. |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (container listens on 8080). |
| resources | object | `{"limits":{"cpu":"500m","ephemeral-storage":"128Mi","memory":"256Mi"},"requests":{"cpu":"50m","ephemeral-storage":"32Mi","memory":"64Mi"}}` | Container resources. ephemeral-storage covers the `tmp` emptyDir mounted at /tmp, which holds only the Go runtime's scratch files. |
| tmpVolume.sizeLimit | string | `"128Mi"` | Size limit of the `tmp` emptyDir mounted at /tmp; the kubelet evicts the pod when the volume grows past it. Empty sets no limit. |
| logging.verbose | bool | `false` | Enable debug logging. |
| observability.otel.endpoint | string | `""` | OTLP collector the server exports its traces to (`OTEL_EXPORTER_OTLP_ENDPOINT`), e.g. `http://otlp-gateway.kube-system.svc:4317` for gRPC. Empty exports nothing; the W3C propagator still honours an inbound `traceparent`. |
| observability.otel.protocol | string | `"grpc"` | OTLP protocol: `grpc` or `http/protobuf` (`OTEL_EXPORTER_OTLP_PROTOCOL`). |
| observability.otel.headers | string | `""` | Extra OTLP headers as `key=value,...` (`OTEL_EXPORTER_OTLP_HEADERS`), e.g. `X-Scope-OrgID=giantswarm` for the collector's tenant. |
| observability.otel.resourceAttributes | string | `""` | Extra resource attributes as `key=value,...`, appended to the pod's `k8s.pod.name`, `k8s.namespace.name` and `k8s.node.name` (`OTEL_RESOURCE_ATTRIBUTES`). |
| observability.otel.sampler | string | `"parentbased_traceidratio"` | Trace sampler (`OTEL_TRACES_SAMPLER`). Parent-based: a call arriving with a sampled `traceparent` (muster's) is always kept, a root span is sampled at `samplerArg`. |
| observability.otel.samplerArg | string | `"0.1"` | Sampler argument (`OTEL_TRACES_SAMPLER_ARG`): the ratio of root spans kept. |
| observability.metrics.enabled | bool | `true` | Serve Prometheus metrics on `GET /metrics` of the `metrics` port: `mcp_server_operation_duration_seconds` per tool, and `http_server_request_duration_seconds` for the REST API and the MCP endpoint. Off, `OTEL_METRICS_EXPORTER=none`: nothing is served, and nothing is pushed to `observability.otel.endpoint` either. |
| observability.metrics.port | int | `9464` | Port of the metrics listener, apart from the API port so an HTTPRoute or the OAuth guard never covers it. |
| serviceMonitor.enabled | bool | `false` | Render a ServiceMonitor for the `metrics` port. Also needs `observability.metrics.enabled`; the meta chart turns this on with the tenant label. |
| serviceMonitor.interval | string | `""` | Scrape interval; empty uses the Prometheus Operator default. |
| serviceMonitor.labels | object | `{}` | Labels on the ServiceMonitor, beside the chart's own. The Giant Swarm observability platform routes a scrape to a Mimir tenant by `observability.giantswarm.io/tenant`; a monitor without it writes to no tenant. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
