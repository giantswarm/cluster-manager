# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `create_node_pool` and `delete_node_pool` in apply mode: the pool release of the `gpu-node-pool` chart (HelmRelease and OCIRepository in `org-<org>`, chart pinned exactly, default 0.3.0) composed from the cluster's Release CR (Kubernetes version and Flatcar machine image, refused when the release runs ahead of the control plane) and a credential-free snapshot of the cluster's values (registry mirrors, proxy, base domain, management cluster, Cilium IPAM mode; registry credentials in a `valuesFrom` Secret), `teleport.enabled` from the join-token Secret's presence, owned by the `Cluster`; idempotent on the same name — the re-run is the update, its dry-run the drift check; a GitOps-owned object is never patched. `delete_node_pool` refuses while the pool runs nodes (named) unless forced and removes only what `create_node_pool` created. Golden compose tests pin the release shape per accelerator and snapshot variant.
- Chart RBAC for the writes (HelmRelease, OCIRepository, Secret in `org-*`) and the snapshot's reads (App, ConfigMap).



[Unreleased]: https://github.com/giantswarm/cluster-manager/tree/main
