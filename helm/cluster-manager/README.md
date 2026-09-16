# cluster-manager

The Agent Platform's MCP-only cluster write surface — the installation's clusters and their GPU node pools, listed and written as the person, exposed to agents and the portal through muster

The chart deploys one Deployment that serves the MCP streamable-HTTP endpoint
under `/mcp` — cluster-manager's only surface. Its tools (`get_info`,
`list_clusters`, `list_node_pools`, `create_node_pool`, `delete_node_pool`,
`enable_model_serving`, `disable_model_serving`) read the
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
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/cluster-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified app name. |
| installation.name | string | `""` | Name of the installation. `list_clusters` reports the Cluster of that name as the installation's own cluster (its management cluster); empty marks none. |
| modelManager.namespace | string | `"agent-platform"` | Namespace model-manager runs in on the installation. `create_node_pool` registers the serving cluster's `kserve` backend there by writing the `model-backend-kserve` ConfigMap of model-manager's runtime-registration contract; `delete_node_pool` of the cluster's last pool removes it; `enable_model_serving` and `disable_model_serving` register and remove it too. |
| serving.namespace | string | `"model-serving"` | Namespace on a serving cluster where the InferenceServices go, named as `target.servingNamespace` in the registered `kserve` backend. |
| serving.sliceChartVersion | string | `""` | Exact `agent-platform` chart version the `<cluster>-agent-platform` slice release pins. Empty pins the version the installation's own platform release runs (at least 4.27.0, the first chart that places the predictors on a tainted GPU pool); set it only for a lab running an unreleased chart. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint (the server's only surface). |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make cluster-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint requires a bearer token the platform identity provider issued, and every call carries the caller's identity. On the Agent Platform muster forwards the session's IdP id_token to this server (MCPServer `auth.forwardToken`, rendered below) and the portal's calls arrive the same way through muster; both are validated against the IdP's JWKS when their audience is in `trustedAudiences`. Off: anonymous, acting as the ServiceAccount — only for a server nothing but a trusted proxy can reach. |
| oauth.baseURL | string | `""` | Public base URL of this server: the issuer of its own OAuth metadata (https, or http on loopback). Empty derives `https://<fullname>.<global.domain>` when `global.domain` is set. |
| oauth.provider | string | `"dex"` | Identity provider: `dex` or `google`. |
| oauth.dex.issuerURL | string | `""` | Dex issuer URL. Empty falls back to `global.identity.issuerUrl`. |
| oauth.dex.clientID | string | `""` | Dex OAuth client ID. Empty falls back to `global.identity.clientId`. |
| oauth.dex.clientSecret | string | `""` | Dex OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.dex.allowPrivateURLs | bool | `false` | Let the issuer resolve to a private or loopback address (an in-cluster Dex). |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Secret with the CA of a Dex that serves a private certificate; mounted and passed as `--dex-ca-file`. Empty name falls back to `global.identity.ca.secretName` / `global.identity.ca.key`. |
| oauth.google.clientID | string | `""` | Google OAuth client ID (not secret; may also come from the Secret key `google-client-id` when empty). |
| oauth.google.clientSecret | string | `""` | Google OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.existingSecret | string | `""` | Existing Secret with the provider credentials: `dex-client-secret` (dex) or `google-client-secret` (+ optional `google-client-id`) (google). Empty falls back to `global.identity.existingSecret`, whose `dex-client-secret` is the platform client's; without that, the chart renders a Secret from the values above. |
| oauth.trustedAudiences | list | `[]` | OAuth client IDs whose IdP id_tokens are accepted as bearer tokens (SSO token forwarding). Empty falls back to `[global.identity.clientId]`, the platform client MCP clients and the muster CLI log in with. The server trusts the union of this list and `muster.mcpServer.auth.requiredAudiences` (in that order, without duplicates): every token muster forwards carries the required audiences by construction and they are what the kube-apiserver trusts, so a portal session — whose id_token carries them but not the platform client — is accepted without listing its client here. |
| oauth.sso.allowPrivateIPs | bool | `false` | Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (an in-cluster Dex). |
| oauth.allowPublicClientRegistration | bool | `false` | Accept unauthenticated dynamic client registration (labs only). |
| oauth.downstream.enabled | bool | `true` | Call the Kubernetes API as the caller: everything a request does presents the caller's IdP token, so the caller's RBAC governs — the apiserver must trust the IdP and the token's audience (a Dex install lists that audience in `muster.mcpServer.auth.requiredAudiences`; a Google install's client id is the apiserver's `--oidc-client-id`). This is cluster-manager's rule (every write as the person), so it is on by default and takes effect with `oauth.enabled`. The ServiceAccount then holds no permissions: the chart renders no RBAC for it (`rbac.create` is moot). |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"cluster-manager"` | MCPServer CR name (drives the tool prefix). |
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
| resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Container resources. |
| logging.verbose | bool | `false` | Enable debug logging. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
