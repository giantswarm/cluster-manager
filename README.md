# cluster-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/cluster-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/cluster-manager/tree/main)

The Agent Platform's MCP-only cluster write surface: node pools first
([bumblebee-plans#46](https://github.com/giantswarm/bumblebee-plans/pull/46),
[giantswarm/giantswarm#37637](https://github.com/giantswarm/giantswarm/issues/37637),
epic [giantswarm/giantswarm#37639](https://github.com/giantswarm/giantswarm/issues/37639)).

cluster-manager is the third sibling of `model-manager` and `agent-manager`: a Go MCP server
(streamable HTTP, an mcp-oauth resource server) registered with muster by its own `MCPServer`
CR. Agents call its tools through muster, and so does the Dev Portal — as the signed-in person.
Every Kubernetes call is presented to the API server with the caller's forwarded IdP token
(`forwardToken`), so the caller's RBAC governs what a tool may read or write. There is no REST
API.

## Tools

Through muster the tools appear as `x_cluster-manager_<tool>`.

| Tool | Purpose |
|---|---|
| `get_info` | Version, mode capabilities (`apply`, `commit`) and the tool list |
| `list_clusters` | The installation's clusters: name, organization, release version, own-cluster flag, GPU operator and serving presence with their provider, the GPU pool releases, the commit target |
| `list_node_pools` | The MachinePools of one cluster with the pool's Kubernetes version and the control plane's as two fields, replicas, instance and accelerator types, the owning HelmRelease |

Write tools (`create_node_pool`, `delete_node_pool`, `enable_model_serving`,
`disable_model_serving`), every one with `dryRun` and `mode: apply | commit`, follow in the
epic's later stages.

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
