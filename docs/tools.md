# The tools

Generated from the registered tools (`make tools-doc`); through muster every tool is `x_cluster-manager_<name>`. Every tool is registered in every mode; mode `commit` needs the server registered with its GitHub App (`get_info` `modes.commit`).

| Tool | Kind |
|---|---|
| `create_cluster` | write |
| `create_node_pool` | write |
| `delete_cluster` | write, destructive |
| `delete_node_pool` | write, destructive |
| `disable_model_serving` | write, destructive |
| `enable_model_serving` | write |
| `get_info` | read-only |
| `list_clusters` | read-only |
| `list_node_pools` | read-only |
| `list_releases` | read-only |
| `remove_model_cache` | write, destructive |

## `create_cluster`

```text
Create a workload cluster in an organization from a Giant Swarm release, as you or, in mode commit, as a pull request.

Composes the release chart's HelmRelease, OCIRepository and <name>-values ConfigMap in org-<organization>, checked against the release chart's schema before any write. The cluster trusts the installation's identity provider, and binds you and the organization's admins as cluster-admin; values with their own global.controlPlane.oidc or bindings win whole. A re-run with the same arguments changes nothing; create_node_pool adds GPU pools.
```

```json
{
  "properties": {
    "description": {
      "description": "A sentence on the cluster's purpose (global.metadata.description)",
      "type": "string"
    },
    "dryRun": {
      "default": false,
      "description": "Return the rendered manifests without writing; in mode commit also the files, the repository, the branch and the directory",
      "type": "boolean"
    },
    "identity": {
      "description": "The cloud identity the cluster runs under, never a credential (aws, eks: AWSClusterRoleIdentity; azure: AzureClusterIdentity); default the chart's",
      "type": "string"
    },
    "mode": {
      "default": "apply",
      "description": "apply: create the objects as you; commit: open the pull request as you in the repository that owns the organization (get_info modes.commit)",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "name": {
      "description": "The cluster's name: a DNS label of at most 20 characters starting with a letter, unused on the installation; it cannot change",
      "type": "string"
    },
    "organization": {
      "description": "The organization the cluster belongs to; it lives in the namespace org-<organization>",
      "type": "string"
    },
    "provider": {
      "description": "The provider line: its release chart is release-<provider>",
      "enum": [
        "aws",
        "azure",
        "eks"
      ],
      "type": "string"
    },
    "release": {
      "description": "The release version (36.0.0); default the newest active release of the provider line — list_releases names them",
      "type": "string"
    },
    "values": {
      "description": "The cluster's further values (global.controlPlane, global.nodePools, …), checked with the composed ones against the release chart's schema",
      "properties": {},
      "type": "object"
    }
  },
  "required": [
    "organization",
    "name",
    "provider"
  ],
  "type": "object"
}
```

## `create_node_pool`

```text
Create or update a GPU node pool for a cluster, with the GPU operator and the model serving slice it needs.

A second call with the same name is the update; dryRun shows the difference. Composes the gpu-node-pool release from the cluster's current release, the <cluster>-gpu-operator release where none runs, and the cluster's one serving slice (<cluster>-agent-platform), shared by every pool and updated on every call; registers the cluster's kserve backend with model-manager. The answer prices each size and names the smallest hosting each serving preset (presetFit).
```

```json
{
  "properties": {
    "accelerator": {
      "default": "nvidia-l4",
      "description": "Accelerator from the curated list; picks the EC2 instance family",
      "enum": [
        "nvidia-l4",
        "nvidia-a10g",
        "nvidia-t4",
        "nvidia-l40s"
      ],
      "type": "string"
    },
    "cache": {
      "default": false,
      "description": "Whether the cluster's slice, for every pool, mounts a model cache claim: a volume billed monthly until remove_model_cache. Omitted: keep the setting",
      "type": "boolean"
    },
    "chartVersion": {
      "description": "Exact gpu-node-pool chart version to pin; default none: the pool follows the chart from 0.8.0 to the next major",
      "type": "string"
    },
    "cluster": {
      "description": "Cluster name",
      "type": "string"
    },
    "dryRun": {
      "default": false,
      "description": "Render and compare only; nothing is written",
      "type": "boolean"
    },
    "maxGpus": {
      "default": 4,
      "description": "Upper bound of the pool in GPUs across all of its nodes; the minimum is always 0 (scale to zero)",
      "minimum": 1,
      "type": "number"
    },
    "mode": {
      "default": "apply",
      "description": "apply: land the objects as you; commit: open the pull request as you (list_clusters commitTarget), the backend registered live",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "name": {
      "description": "Pool name within the cluster, five to twenty lowercase characters, digits and dashes (gpu00, gpu-l4); <cluster>-<name> names the MachinePool",
      "pattern": "^[a-z0-9][-a-z0-9]{3,18}[a-z0-9]$",
      "type": "string"
    },
    "namespace": {
      "description": "Cluster namespace (org-<organization>); optional when the name is unique on the installation",
      "type": "string"
    },
    "prewarm": {
      "default": false,
      "description": "Launch the pool's first node with the release, held by a placeholder until the first workload; only for the installation's own pool, at install",
      "type": "boolean"
    },
    "serving": {
      "default": true,
      "description": "Whether the pool serves models: false creates a plain GPU pool, with no serving slice and no backend registration",
      "type": "boolean"
    },
    "sizes": {
      "description": "Instance sizes Karpenter may pick within the family, smallest first; default the chart's: xlarge, 2xlarge, 4xlarge",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "zones": {
      "description": "Availability zones of the cluster's node subnets (list_clusters zones). With cache on, every pool's predictors then mount the named zone's claim",
      "items": {
        "type": "string"
      },
      "type": "array"
    }
  },
  "required": [
    "cluster",
    "name"
  ],
  "type": "object"
}
```

## `delete_cluster`

```text
Delete a workload cluster create_cluster created, with its GPU pools and serving, in two calls with the same arguments.

The first call removes its model backend registration, HelmRelease, OCIRepository and values, without waiting for the uninstall; nextStep says when to re-run. Once list_clusters no longer lists it, the second call removes what the uninstall left, then answers notFound.nothingLeft. Refused with the way out for a cluster cluster-manager did not create, the installation's own, and one Flux applies from git. Served models are no refusal: they go with it (dryRun names them).
```

```json
{
  "properties": {
    "dryRun": {
      "default": false,
      "description": "List what would be removed; nothing is written; in mode commit also the files, the repository, the branch and the directory",
      "type": "boolean"
    },
    "mode": {
      "default": "apply",
      "description": "apply: remove the objects as you; commit: open the removal pull request as you, the backend removed live; commit.liveSteps names what is left",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "name": {
      "description": "The cluster's name as given to create_cluster",
      "type": "string"
    },
    "organization": {
      "description": "The organization the cluster belongs to; it lives in the namespace org-<organization>",
      "type": "string"
    }
  },
  "required": [
    "organization",
    "name"
  ],
  "type": "object"
}
```

## `delete_node_pool`

```text
Delete a GPU node pool create_node_pool created, idle nodes first; with the cluster's last pool, its model serving too.

Refused while a node of the pool is busy unless force, naming each busy node and the models to unload first with model-manager's unload_model, and while a served model waits for a node of this pool. Does not wait for the instances to terminate: list_node_pools shows them. With a pool that is not the last, the backend registration is re-written for the pools left.
```

```json
{
  "properties": {
    "cluster": {
      "description": "Cluster name",
      "type": "string"
    },
    "dryRun": {
      "default": false,
      "description": "List what would be removed; nothing is written",
      "type": "boolean"
    },
    "force": {
      "default": false,
      "description": "Delete even while the pool runs nodes: their workloads, served models included, are evicted",
      "type": "boolean"
    },
    "mode": {
      "default": "apply",
      "description": "apply: remove the objects as you, refused for a pool Flux applies from git; commit: open the removal pull request, the backend changed live",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "name": {
      "description": "Pool name as given to create_node_pool",
      "type": "string"
    },
    "namespace": {
      "description": "Cluster namespace (org-<organization>); optional when the name is unique on the installation",
      "type": "string"
    }
  },
  "required": [
    "cluster",
    "name"
  ],
  "type": "object"
}
```

## `disable_model_serving`

```text
Switch model serving off for a cluster: remove the serving slice and model backend cluster-manager created.

Refused while models are served on the cluster unless force (the message names them), when the cluster cannot be read as you, and for a slice cluster-manager did not create. The slice goes in order; last the serving namespace is removed, or marked retired, naming what holds it. The model cache claims stay: remove_model_cache removes them.
```

```json
{
  "properties": {
    "cluster": {
      "description": "Cluster name",
      "type": "string"
    },
    "dryRun": {
      "default": false,
      "description": "List what would be removed; nothing is written",
      "type": "boolean"
    },
    "force": {
      "default": false,
      "description": "Remove the serving slice even while models are served: they go with it",
      "type": "boolean"
    },
    "mode": {
      "default": "apply",
      "description": "apply: remove the objects from the installation as you; commit is refused here (get_info modes.commitTools)",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "namespace": {
      "description": "Cluster namespace (org-<organization>); optional when the name is unique on the installation",
      "type": "string"
    }
  },
  "required": [
    "cluster"
  ],
  "type": "object"
}
```

## `enable_model_serving`

```text
Switch model serving on for a cluster, with or without a GPU pool: its one serving slice, <cluster>-agent-platform.

The slice (KServe, the llm-d control plane, the models Gateway at models.<domain>) follows the agent-platform chart from the version the installation's platform release runs, at least 4.85.0, to the next major; the cluster's kserve backend is registered with model-manager. An existing release is updated in place. Refused where the platform's own release or a hand install provides serving already.
```

```json
{
  "properties": {
    "cache": {
      "default": false,
      "description": "Whether the slice mounts a model cache claim: a volume billed monthly until remove_model_cache. Omitted: keep the release's setting",
      "type": "boolean"
    },
    "cluster": {
      "description": "Cluster name",
      "type": "string"
    },
    "dryRun": {
      "default": false,
      "description": "Render and compare only; nothing is written",
      "type": "boolean"
    },
    "mode": {
      "default": "apply",
      "description": "apply: land the objects on the installation as you; commit is refused here (get_info modes.commitTools)",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "namespace": {
      "description": "Cluster namespace (org-<organization>); optional when the name is unique on the installation",
      "type": "string"
    }
  },
  "required": [
    "cluster"
  ],
  "type": "object"
}
```

## `get_info`

```text
Report this server's version, write modes (apply, commit), tools, and whether the installation serves the Cluster API.

apply lands objects on the installation as you; commit opens a pull request as you. Without the Cluster API (cluster.x-k8s.io), list_clusters is empty and the tools naming a cluster refuse.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `list_clusters`

```text
List the installation's clusters with their organization, release, GPU operator, model serving, zones and model cache.

Says who provides the GPU operator and serving (chart, cluster-manager, manual) and how ready they are, the zones a pool may be pinned to, and what each model cache claim costs a month. Empty where the installation does not serve the Cluster API.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `list_node_pools`

```text
List one cluster's node pools with their lifecycle, nodes, Kubernetes versions, instance sizes and prices.

phase is creating, ready, scaling, removing or failed; steps name what holds a pool or node back (a drain, a launch Karpenter refused, and the way around it). The pool's and the control plane's Kubernetes versions are given side by side, without a verdict.
```

```json
{
  "properties": {
    "cluster": {
      "description": "Cluster name",
      "type": "string"
    },
    "namespace": {
      "description": "Cluster namespace (org-<organization>); optional when the name is unique on the installation",
      "type": "string"
    }
  },
  "required": [
    "cluster"
  ],
  "type": "object"
}
```

## `list_releases`

```text
List the installation's Giant Swarm releases, newest first per provider line, and whether create_cluster offers each.

Each with its state (active, deprecated, wip, preview), Kubernetes version, cluster chart and release chart. offered is true for an active release whose release chart is published, on a provider line create_cluster supports; note says why not. Without the Release CRD the list is empty and a note names it.
```

```json
{
  "properties": {
    "provider": {
      "description": "Only this provider line's releases (aws, eks, azure, …); default every line",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `remove_model_cache`

```text
Remove a cluster's model cache: its hf-cache* storage claims, billed every month they exist, even with no pool left.

Where cluster-manager's serving slice mounts the cache, it is switched to serve without it first; every predictor then downloads its weights at each start. Refused while a pod mounts a claim to be removed (the refusal names the models to unload with model-manager's unload_model), when the cluster cannot be read as you, and where someone else provides serving.
```

```json
{
  "properties": {
    "claim": {
      "description": "One model cache claim to remove, by name (hf-cache, hf-cache-eu-central-1a); default every hf-cache* claim of the serving namespace",
      "type": "string"
    },
    "cluster": {
      "description": "Cluster name",
      "type": "string"
    },
    "dryRun": {
      "default": false,
      "description": "List what would be removed; nothing is written",
      "type": "boolean"
    },
    "mode": {
      "default": "apply",
      "description": "apply: remove the objects from the installation and the cluster as you; commit is refused here (get_info modes.commitTools)",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "namespace": {
      "description": "Cluster namespace (org-<organization>); optional when the name is unique on the installation",
      "type": "string"
    }
  },
  "required": [
    "cluster"
  ],
  "type": "object"
}
```
