# cluster-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/cluster-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/cluster-manager/tree/main)

The Agent Platform's MCP-only cluster write surface: node pools first
([bumblebee-plans#46](https://github.com/giantswarm/bumblebee-plans/pull/46),
[giantswarm/giantswarm#37637](https://github.com/giantswarm/giantswarm/issues/37637),
epic [giantswarm/giantswarm#37639](https://github.com/giantswarm/giantswarm/issues/37639)).

cluster-manager is the third sibling of `cluster-manager` and `agent-manager`: a Go MCP server
(streamable HTTP, an mcp-oauth resource server) registered with muster by its own `MCPServer`
CR. Agents call its tools through muster, and so does the Dev Portal — as the signed-in person.
Every Kubernetes call is presented to the API server with the caller's forwarded IdP token
(`forwardToken`), so the caller's RBAC governs what a tool may read or write. There is no REST
API.

## Tools

Through muster the tools appear as `x_cluster-manager_<tool>`.

| Tool | Purpose |
|---|---|
| `get_info` | Version, mode capabilities (`apply`, `commit`), the tool list and whether the installation serves the Cluster API (`clusterApi`) |
| `list_clusters` | The installation's clusters: name, organization, release version, own-cluster flag, GPU operator and serving presence with their provider, the GPU pool releases, the commit target; empty with a `clusterApi` note on an installation without the Cluster API |
| `list_node_pools` | The MachinePools of one cluster with the pool's Kubernetes version and the control plane's as two fields, replicas, instance and accelerator types, the owning HelmRelease |
| `create_node_pool` | Create a GPU node pool for a cluster, or update the pool of that name: the pool release of the `gpu-node-pool` chart (HelmRelease and OCIRepository in `org-<org>`, chart pinned exactly) with the Kubernetes version and Flatcar machine image of the cluster's current release — refused when the release runs ahead of the control plane — and a credential-free snapshot of the cluster's settings (registry credentials go into a `valuesFrom` Secret). `mode: apply` lands the objects as the caller, owned by the `Cluster`; `dryRun` returns the manifests and, on a re-run, the difference. A GitOps-owned object is never patched. When no GPU operator runs on the cluster (detected as the caller on the cluster itself), the `<cluster>-gpu-operator` release of the catalog's `gpu-operator` chart comes with the pool, configured from the two-row table read off the nodes (Flatcar: driver and toolkit off; `nvidia.com/gpu.deploy.driver=pre-installed`: toolkit on; anything else refused); an operator the platform's release or a person provides is never re-created. When nothing provides serving on the cluster, the cluster's `<cluster>-agent-platform` slice release comes with the pool too (see `enable_model_serving`), updated in place on a re-run. After the pool, the cluster's `kserve` backend is registered with model-manager. |
| `delete_node_pool` | Remove what `create_node_pool` created. Refused while the pool still runs nodes (named) unless `force`; refused for a pool cluster-manager did not create. With the cluster's last pool go the operator release, the slice release — unless another slice is on in it (`sliceKept`) — and the backend registration cluster-manager created. |
| `enable_model_serving` | Switch model serving on for a cluster, with or without a GPU pool: the cluster's **one** release of the `agent-platform` chart, `<cluster>-agent-platform` (HelmRelease and OCIRepository in `org-<org>`, delivered by the installation's Flux; the chart pinned exactly to the version the installation's own platform release runs — `status.history[0].chartVersion` of its HelmRelease, released by construction and known to work on the installation — with a floor of 4.27.0, the first chart whose serving slice places the predictors on a tainted GPU pool; a platform below it is refused, naming why; `--slice-chart-version` / `serving.sliceChartVersion` pins another version for a lab), with the serving slice on — the five KServe components and the llm-d well-known configs, `modelServing` with the `nvidia` RuntimeClass, the models Gateway at `models.<domain>` with the login issuer's JWT policy. `global.domain`, `global.identity` and the wildcard certificate where the platform names one, else a Certificate of `models.<domain>` from the fleet's ClusterIssuer — `--models-certificate-issuer` / `serving.certificateIssuer`, default `letsencrypt-giantswarm` are read from the installation's own platform release, never invented; every cluster gets the target knob (`gitops.target.kubeConfig.secretRef.name: <cluster>-kubeconfig`, see Delivery below); a workload cluster gets `global.domain: <cluster>.<base domain>` and `components.agentgateway` on, the installation's own cluster the platform's domain and agentgateway off (the platform's release owns the data plane); the chart's Flux engine and model-manager are always off. With exactly one GPU pool on the cluster the predictors are pinned to it (`modelServing.gpuPool.nodeSelector`, and `modelServing.serving.nodeSelector` for the pinned chart). A release that exists is updated in place — the re-run moves the pin to the version the platform runs today; a release of the chart under another name is refused, naming it; a chart-provided or hand-installed serving layer is left alone. Registers the `kserve` backend. |
| `disable_model_serving` | Remove the slice release cluster-manager created and its backend registration. Refused while models are served on the cluster (named) unless `force`, and plainly when the cluster cannot be read as the caller; refused for a release cluster-manager did not create. |

Every write tool takes `dryRun` (the rendered manifests, or on a re-run the difference) and
`mode: apply | commit`; `commit` (a pull request as the caller) follows in the epic's later stage.

## Delivery

Giant Swarm installations enforce Flux multi-tenancy on every HelmRelease outside the platform's
namespaces (the `flux-multi-tenancy` Kyverno policy, admission-enforced, on `org-<org>` among
others): a release carries `spec.serviceAccountName` or `spec.kubeConfig.secretRef.name`, and leaves
its namespace (`targetNamespace`, `storageNamespace`) only through a kubeconfig. The releases
cluster-manager composes take the two shapes the fleet's own releases in `org-<org>` take, by what
they render — for the installation's own cluster exactly as for a workload cluster:

| release | objects live | delivery |
|---|---|---|
| `<cluster>-gpu-operator` | on the cluster, in `kube-system` | `kubeConfig.secretRef.name: <cluster>-kubeconfig` — the cluster's Cluster API kubeconfig Secret, which exists for the installation's own cluster too and points at its own API server |
| `<cluster>-<pool>` | in `org-<org>` on the installation (the Cluster API objects) | `serviceAccountName: automation` — the org's tenant ServiceAccount (`--tenant-service-account` / `flux.tenantServiceAccount`; empty renders none, for a lab without the policy) |
| `<cluster>-agent-platform` | in `org-<org>` on the installation (the meta chart's child HelmReleases and OCIRepositories) | `serviceAccountName: automation`; the target knob `gitops.target.kubeConfig.secretRef.name: <cluster>-kubeconfig` in its values puts the kubeconfig on every child, which then installs into the cluster |

The dry run cannot see admission, so the goldens hold the policy's constraint on every composed
HelmRelease.

## Running

```sh
cluster-manager serve --kubeconfig ~/.kube/config
```

Every flag has an environment variable named next to it in `--help`; flags win. The chart in
`helm/cluster-manager` renders the Deployment, the Service, the `MCPServer` CR for muster and,
with `oauth.enabled`, the mcp-oauth resource-server flags from the platform identity contract
(`global.identity`).

## Development

```sh
make build-linux-amd64   # the binary the Dockerfile expects
go test ./...
make helm-lint helm-template
```

Releases are automatic: every merge to `main` is tagged from Conventional Commits and CircleCI
publishes the image to `gsoci.azurecr.io/giantswarm/cluster-manager` and the chart to the
Giant Swarm catalog.
