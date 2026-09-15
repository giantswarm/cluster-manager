# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `create_node_pool` and `delete_node_pool` in apply mode: the pool release of the `gpu-node-pool` chart (HelmRelease and OCIRepository in `org-<org>`, chart pinned exactly, default 0.3.0) composed from the cluster's Release CR (Kubernetes version and Flatcar machine image, refused when the release runs ahead of the control plane) and a credential-free snapshot of the cluster's values (registry mirrors, proxy, base domain, management cluster, Cilium IPAM mode; registry credentials in a `valuesFrom` Secret), `teleport.enabled` from the join-token Secret's presence, owned by the `Cluster`; idempotent on the same name — the re-run is the update, its dry-run the drift check; a GitOps-owned object is never patched. `delete_node_pool` refuses while the pool runs nodes (named) unless forced and removes only what `create_node_pool` created. Golden compose tests pin the release shape per accelerator and snapshot variant.
- Chart RBAC for the writes (HelmRelease, OCIRepository, Secret in `org-*`) and the snapshot's reads (App, ConfigMap).
- Detection of the GPU operator and the serving layer per cluster, reported by `list_clusters` with the provider (`chart`: the platform's own release; `cluster-manager`; `manual`: by hand) and the evidence: a HelmRelease `gpu-operator` rendered by an agent-platform release, any HelmRelease or App of the `gpu-operator` chart, a `ClusterPolicy`, GPU node labels; the KServe and llmisvc APIs and the model-serving discovery ConfigMap. A workload cluster is read through its own apiserver as the caller (apiserver and CA from its kubeconfig Secret's cluster section, the caller's forwarded token as identity — never the kubeconfig's credentials); `unknown` with the reason when it cannot be.
- `create_node_pool` composes the `<cluster>-gpu-operator` release (OCIRepository following the catalog's `gpu-operator` 1.x, HelmRelease into `kube-system`, `spec.kubeConfig.secretRef` → `<cluster>-kubeconfig` on a workload cluster, labelled `giantswarm.io/cluster`) when no operator runs, configured from the two-row table read off the cluster's nodes — Flatcar: driver and toolkit off; `nvidia.com/gpu.deploy.driver=pre-installed`: toolkit on; anything else refused, naming the OS images and `nvidia.com` labels seen and the two rows. A chart-provided or manual operator is never re-created or edited; cluster-manager's own is updated on the re-run.
- `create_node_pool` registers the cluster's `kserve` backend with model-manager (the `model-backend-kserve` ConfigMap of the runtime-registration contract in model-manager's namespace, `target.{cluster, organization, apiServer, caBundle, servingNamespace}`; `local` for the installation's own cluster), idempotently; refused when the document is registered for another cluster. `delete_node_pool` of the cluster's last pool removes the operator release and the backend document cluster-manager created (`lastPool`).
- Chart values `modelManager.namespace` and `serving.namespace`; RBAC for the detection's reads (nodes, ClusterPolicies, KServe APIs) and the backend ConfigMap's writes.



[Unreleased]: https://github.com/giantswarm/cluster-manager/tree/main
