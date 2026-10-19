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

Composes the release chart's HelmRelease, OCIRepository and <name>-values ConfigMap in org-<organization>, checked against the release chart's schema before any write. The cluster trusts the installation's identity provider, and binds you and the organization's admins as cluster-admin; values with their own global.controlPlane.oidc or bindings win whole. Each refusal says why and the way out. A re-run with the same arguments changes nothing; create_node_pool adds GPU pools.
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

A second call with the same name is the update; dryRun shows the difference. Composes the gpu-node-pool release from the cluster's current release and, where missing, the <cluster>-gpu-operator release and the cluster's one serving slice (<cluster>-agent-platform), and registers the cluster's kserve backend with model-manager. The answer lists each size with what it leaves a predictor, its local disk and on-demand price, and the smallest size that hosts each serving preset (presetFit).
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
      "description": "Availability zones the pool's nodes may launch in, of the cluster's node subnets (list_clusters zones); with cache on, the pool follows its cache",
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

The first call removes its model backend registration, HelmRelease, OCIRepository and values, without waiting for the uninstall; the answer names what goes with it, the models served and nextStep. Once list_clusters no longer lists it, the second call removes what the uninstall left, and then answers not found with notFound.nothingLeft. Refused with the way out for a cluster cluster-manager did not create, the installation's own, and one Flux applies from git.
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

Refused while a node of the pool is busy unless force, naming each busy node and the models to unload first with model-manager's unload_model, and while a served model waits for a node of this pool. Does not wait for the instances to terminate: list_node_pools shows them. With a pool that is not the last, the backend registration is re-written for the pools left. Answers within the caller's deadline: partial and nextStep name the re-run that continues.
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

Refused while models are served on the cluster unless force (the message names them), when the cluster cannot be read as you, and for a slice cluster-manager did not create. The slice goes in order; last the serving namespace is removed, or marked retired, naming what holds it. The model cache claims stay: remove_model_cache removes them. Answers within the caller's deadline: partial and nextStep name the re-run that continues. dryRun lists what would be removed.
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

The slice (KServe, the llm-d control plane, the models Gateway at models.<domain>) follows the agent-platform chart from the version the installation's platform release runs, at least 4.85.0, to the next major; the cluster's kserve backend is registered with model-manager. An existing release is updated in place. Refused where the platform's own release or a hand install provides serving already. dryRun returns the rendered manifests and writes nothing.
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

Per cluster: whether it is the installation's own; who provides the GPU operator and the serving layer (chart, cluster-manager, manual or unknown) and their readiness; the zones of its node subnets a pool may be pinned to; its GPU pool releases; where commit mode writes (commitTarget); its model cache claims with their zone and monthly price; and for a cluster create_cluster made, who it binds as cluster-admin. Empty where the installation does not serve the Cluster API.
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

phase is creating, ready, scaling, removing or failed; steps say where the pool release, the MachinePool, the nodes and a prewarm placeholder stand, naming what holds a node back (the pods a drain waits on, a launch Karpenter refused and the way around it) and the idle nodes delete_node_pool removes. The pool's and the control plane's Kubernetes versions are given side by side, without a verdict. Each size carries its on-demand price per hour in the cluster's region, or priceNote.
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
Remove a cluster's model cache: its hf-cache* volume claims, billed every month they exist, even with no pool left.

Where cluster-manager's serving slice mounts the cache, it is switched to serve without it first; every predictor then downloads its weights at each start. Refused while a pod mounts a claim to be removed (the refusal names the models to unload with model-manager's unload_model), when the cluster cannot be read as you, and where someone else provides serving. The answer lists the claims removed with their size and monthly price. dryRun lists what would be removed.
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
