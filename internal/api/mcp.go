// Package api registers cluster-manager's MCP tools. MCP is the only surface:
// agents call the tools through muster, and so does the Dev Portal, as the
// signed-in person (bumblebee-plans#46 D2).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/tools"
)

// MCP tool names. Through muster they appear as x_<server>_<tool>, e.g.
// x_cluster-manager_list_clusters.
const (
	ToolGetInfo        = "get_info"
	ToolListClusters   = "list_clusters"
	ToolListNodePools  = "list_node_pools"
	ToolCreateNodePool = "create_node_pool"
	ToolDeleteNodePool = "delete_node_pool"

	ToolEnableModelServing  = "enable_model_serving"
	ToolDisableModelServing = "disable_model_serving"
	ToolRemoveModelCache    = "remove_model_cache"
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{ToolGetInfo, ToolListClusters, ToolListNodePools, ToolCreateNodePool, ToolDeleteNodePool, ToolEnableModelServing, ToolDisableModelServing, ToolRemoveModelCache}
}

const (
	argCluster      = "cluster"
	argNamespace    = "namespace"
	argName         = "name"
	argAccelerator  = "accelerator"
	argSizes        = "sizes"
	argMaxGPUs      = "maxGpus"
	argChartVersion = "chartVersion"
	argForce        = "force"
	argMode         = "mode"
	argDryRun       = "dryRun"
	argPrewarm      = "prewarm"
	argZones        = "zones"
	argCache        = "cache"
	argClaim        = "claim"

	defaultMaxGPUs = 4
)

// Info is get_info's answer.
type Info struct {
	Version string `json:"version"`
	// Modes are the write modes this server offers: apply lands objects on
	// the installation as the caller; commit opens a pull request as the
	// caller (after the epic's first proof, bumblebee-plans#46 D11).
	Modes Modes    `json:"modes"`
	Tools []string `json:"tools"`
	// ClusterAPI says whether the installation serves the Cluster API the
	// cluster tools read from (absent on an installation without the KaaS
	// components: list_clusters is empty, the cluster-naming tools refuse).
	ClusterAPI tools.ClusterAPI `json:"clusterApi"`
}

// Modes are the write-mode capabilities.
type Modes struct {
	Apply  bool `json:"apply"`
	Commit bool `json:"commit"`
}

// NewMCPServer builds the MCP server exposing the tools. Results are JSON
// text.
func NewMCPServer(svc *tools.Service, version string) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("cluster-manager", version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the clusters of this Giant Swarm installation and their GPU node pools. list_clusters names every cluster with its organization, release, whether it is the installation's own cluster, whether the GPU operator and the serving layer are present and who provides them, and its GPU pool releases; list_node_pools shows one cluster's MachinePools with the pool's Kubernetes version and the control plane's side by side; create_node_pool composes a GPU pool's release (gpu-node-pool chart) from the cluster's current release and settings and lands it as you — a second call on the same name is the update, dryRun the drift check; delete_node_pool removes what create_node_pool created, the pool's idle nodes first, and refuses while a node of the pool is busy; enable_model_serving and disable_model_serving switch the serving slice (KServe, the models Gateway) on or off for a cluster through its one <cluster>-agent-platform release, with or without a GPU pool; remove_model_cache removes a cluster's model cache — the claims that outlive every pool and are billed while they exist — after switching the slice to serve without it. Every call runs as you: what you may read is what these tools list, what you may write is what they change."),
	)
	t := &handlers{svc: svc, version: version}

	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Report this server's version, the write modes it offers (apply: objects landed on the installation as the caller; commit: a pull request opened as the caller), the names of its tools, and whether the installation serves the Cluster API (cluster.x-k8s.io) the cluster tools read from — absent on an installation without the KaaS components, where list_clusters is empty and the tools naming a cluster refuse."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListClusters,
		mcp.WithDescription("List the installation's clusters with the GPU operator's and the serving layer's readiness as structured fields, and the availability zones of the cluster's node subnets (zones: what a pool may be pinned to, the zones create_node_pool checks its zones against; zonesNote when they cannot be read) (gpuOperator.readiness: the operator release's Ready condition, the ClusterPolicy's state, the device plugin and GPU feature discovery DaemonSets' scheduled and ready pods — 0/0 at scale-to-zero; serving.readiness: the slice release and every child release with its Ready condition, the KServe controllers' available replicas, the LLMInferenceServiceConfigs count, the kserve backend registered with model-manager, the published presets count, the models Gateway's Programmed condition, the model cache claim of the serving namespace with the zone its volume is bound to — where every GPU pool created while the claim exists lands — and its monthly price from its size and tier, the tier read from its StorageClass or, the class gone, from the claim's tier annotations (tierSource)): name, organization and namespace, Giant Swarm release version, whether the cluster is the installation's own (its management cluster), whether the GPU operator and the serving layer are present and who provides them — chart (the platform's own release), cluster-manager, manual (by hand) — with the evidence, or unknown with the reason when the cluster cannot be read as you (serving is present with a KServe controller on the cluster, never with the KServe CRDs alone, which Helm leaves behind when a serving layer goes: those are absent with the served APIs noted); the GPU pool releases (HelmReleases of the gpu-node-pool chart) and the commit target (the git repository and path owning the cluster, null when none). Nothing the portal's Clusters pages already show. On an installation that does not serve the Cluster API (cluster.x-k8s.io) the list is empty and clusterApi says so."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listClusters)

	s.AddTool(mcp.NewTool(ToolListNodePools,
		mcp.WithDescription("List the pools of one cluster with their lifecycle: phase (creating, ready, scaling, removing, failed) and steps — the pool release Ready, the MachinePool ready, the nodes (NodeClaims launching, ready, terminating — a terminating node named with its deletion time while Karpenter drains it and terminates its instance; a node Karpenter could not launch named with its refusal — every size refused, the zones tried, the zones AWS named as having the capacity when Karpenter's message still carries them, Karpenter's words verbatim — and the way around it: for a pool pinned to zones, the pin, the model cache claim living in the pinned zone when one does, and the re-run of create_node_pool that moves it (zones naming one zone with capacity — its slice then mounts that zone's claim —, or cache false), else wider sizes or another accelerator; on a GPU pool of cluster-manager's the ready nodes that hold nothing named idle since their last pod left, which delete_node_pool removes with the pool, and a MachinePool still listing instances the cluster no longer has said so), and for a pool created with prewarm the placeholder (prewarm: pending until the pool release installs its Job, then the Job's pod pending while the first node launches, holding a node, preempted by the first workload, finished when the hold ended, or absent after its TTL — the step never decides the phase) — each pending, inProgress, done or failed with since/finishedAt timestamps; a pool release whose MachinePool is not created yet is listed creating, a pool under delete_node_pool stays listed removing with deleting: true and the teardown's pending objects until its HelmRelease is gone, and by its MachinePool, removing with the terminating node named, until the NodeClaim has gone with the terminated instance; plus the pool's Kubernetes version and the control plane's as two fields (no verdict is drawn), replicas and ready replicas, the instance types when readable, the accelerator of the owning pool release and its sizes (sizes: each size as AWS lists it, what it leaves a predictor, and its on-demand price per hour in the cluster's region — pricePerHourUSD, priceSource, priceAsOf, or priceNote saying why there is none), and the HelmRelease that owns the pool (null for a pool created by other means)."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listNodePools)

	s.AddTool(mcp.NewTool(ToolCreateNodePool,
		mcp.WithDescription("Create a GPU node pool for a cluster, or update the pool of that name: composes the pool's release of the gpu-node-pool chart (a HelmRelease and its OCIRepository in the cluster's org- namespace) with the Kubernetes version and Flatcar machine image of the cluster's current release — refused when the release runs ahead of the control plane — and a credential-free snapshot of the cluster's settings (registry mirrors, proxy, base domain, management cluster, Cilium IPAM mode; registry credentials go into a valuesFrom Secret). Where no GPU operator runs it composes the <cluster>-gpu-operator release; where nothing provides serving it composes the cluster's <cluster>-agent-platform release with the serving slice on (KServe, the nvidia RuntimeClass, the models Gateway at models.<domain>; domain, identity and the chart version read from the platform's own release), updated in place on a re-run — the re-run moves the pin to the version the platform runs today; it registers the cluster's kserve backend with model-manager. Mode apply lands the objects on the installation as you, owned by the Cluster so they go with it; a second call on the same name is the update, and its dryRun shows the difference. dryRun returns the rendered manifests and touches nothing. An object of that name someone else owns (GitOps) is never patched. The answer lists the pool's sizes (sizes) with what each leaves a predictor after the node's kubelet reservations and daemonsets, its local NVMe instance store (instanceStoreGB across instanceStoreDisks devices of instanceStoreDiskGB; a pool node's /var/lib — the image unpacks, the kubelet, the pods' emptyDirs and writable layers — is the first device, so instanceStoreDiskGB is what the node gives its pods as ephemeral storage: 250 GB on an xlarge, 450 on a 2xlarge, 600 on a 4xlarge, 450 of a g6 or g6e 8xlarge's two) and its on-demand price per hour in the cluster's region (pricePerHourUSD with priceSource and priceAsOf, from AWS's on-demand Linux list price; a region the price table does not cover, or a size not offered there, carries priceNote instead — never a guess), and the smallest size that hosts each serving preset (presetFit): the presets published on the cluster where a serving layer publishes them (presetFit.origin published), else the ones the slice would publish — the connectivity chart's shipped set at the version the slice's agent-platform release resolves, read from the registry (origin chart; presetFit.source names the chart and version) — so a first pool's size can be chosen by model before the slice exists; each preset carries its displayName and model id; a preset the accelerator could serve but no size of the pool hosts is a warning (warnings) — its predictor would sit Pending while Karpenter refuses every size. With prewarm the installation's own pool launches its first node with the release (a placeholder Job holding one GPU until the first workload preempts it), so a model named up front is served minutes earlier; refused for a workload cluster's pool. Where the pool runs is the person's, and the zone brings its own cache: zones naming one availability zone pins the pool's nodes to it (the chart's pool.zones — placeholder and predictors alike come up there), checked against the cluster's node subnets (a zone the cluster has no node subnet in is refused, naming the zones it has), and the pool's serving slice mounts that zone's model cache claim — the claim Bound there, else hf-cache-<zone> (modelServing.cache.pvc.name on the <cluster>-agent-platform release), which the connectivity chart creates and keeps: the first predictor binds it to a volume in the zone, a later pool in the zone reuses it, pools in other zones use theirs. Several zones with the cache on stand, and the pool follows its cache: the nodes are pinned to the zones named and the slice mounts the base claim (hf-cache), which the first predictor binds to a volume in its node's zone — one of the named — so that from then on every predictor mounting it runs there, and a re-run of the same create pins the pool's nodes to that zone (zonesNote says so); a claim Bound in one of the named zones already pins the pool to it at once, zonesNote naming the zones named and the one that stands; claims Bound in several of the named zones are refused (refused{cacheClaims{claims, remedies}}), as is the base claim Bound outside every named zone, and a claim named after the one zone named but Bound elsewhere (refused{cacheZone{claim, claimZone, zones, remedies}}). Without zones the claims of the serving namespace decide: one claim — Bound, the nodes are pinned to its zone and the slice mounts it; not Bound or naming no zone, nothing is pinned and zonesNote says so; not readable as you, nothing is pinned and a warning says why —; no claim, no pin; several claims are refused naming each with its zone and phase (refused{cacheClaims{claims, remedies}}): name zones. The answer names the pin and the claim (zones, zonesNote, cache{enabled, claim, note}, cacheClaim as read — null while the zone's claim does not exist yet —, cacheClaims: every claim of the namespace with its zone and phase); dryRun shows it. The slice release is the cluster's one: its predictors mount the claim the last create_node_pool with the cache on named. cache is whether this pool's serving slice mounts a claim at all — without it the slice keeps its setting where the cluster's slice release runs, and a first slice serves without a cache (default false): a claim is a volume billed every month it exists, never created unasked; true is the opt-in, the slice's upgrade where it ran without one; false composes modelServing.cache.enabled false on the <cluster>-agent-platform release — no claim is applied or mounted, every predictor downloads its weights into its pod's ephemeral storage (the node's local disk), no zone follows from a claim, and the existing claims are left as they are (Helm keeps them) — and the answer says so; a re-run with a changed value is the slice's upgrade; refused where someone else provides serving. A family not offered in a pinned zone fails its launch, named in list_node_pools' nodes step with what pinned the pool. LLMInferenceServiceConfigs a serving layer that went left terminating in the release namespace are removed first, finalizer and all, when no llm-d controller runs (objects lists them, warnings says why), so the slice's release creates them afresh instead of adopting and losing them."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argName, mcp.Required(), mcp.Pattern(compose.PoolNamePattern.String()), mcp.Description("Pool name within the cluster, five to twenty lowercase characters, digits and dashes (gpu00, gpu-l4); <cluster>-<name> names the MachinePool")),
		mcp.WithString(argAccelerator, mcp.Enum(compose.Accelerators...), mcp.DefaultString(compose.Accelerators[0]), mcp.Description("Accelerator from the curated list; picks the EC2 instance family")),
		mcp.WithArray(argSizes, mcp.Items(map[string]any{"type": "string"}), mcp.Description("Instance sizes Karpenter may pick within the family, smallest first (xlarge, 2xlarge, ...; a size the family does not have is refused); default the chart's: xlarge, 2xlarge, 4xlarge")),
		mcp.WithNumber(argMaxGPUs, mcp.Min(1), mcp.DefaultNumber(defaultMaxGPUs), mcp.Description("Upper bound of the pool in GPUs across all of its nodes; the minimum is always 0 (scale to zero)")),
		mcp.WithString(argChartVersion, mcp.DefaultString(compose.DefaultPoolChartVersion), mcp.Description("Exact gpu-node-pool chart version to pin; default the newest released")),
		mcp.WithBoolean(argPrewarm, mcp.DefaultBool(false), mcp.Description("Launch the pool's first node with the release instead of with the first predictor (pool.prewarm.enabled): a one-shot placeholder Job in the release namespace holds one GPU at negative priority until the first workload preempts it or its hold (the chart's default, 15 minutes) ends and Karpenter consolidates the empty node. Only for the installation's own pool — the Job runs on the installation, where only that pool's nodes join; refused for a workload cluster's pool, naming why. The Job is created with the release's install only: a re-run on an existing pool flips the value but never launches a placeholder again, and a re-run with prewarm false removes the block. list_node_pools shows the placeholder as the prewarm step.")),
		mcp.WithArray(argZones, mcp.Items(map[string]any{"type": "string"}), mcp.Description("The availability zones the pool's nodes may launch in, any combination of the zones of the cluster's node subnets (pool.zones; [\"eu-central-1a\"], [\"eu-central-1a\", \"eu-central-1c\"]; list_clusters names them per cluster) — a zone the cluster has no node subnet in is refused, naming the zones it has. With the cache on and one zone the pool's slice mounts that zone's model cache claim (the claim Bound there, else hf-cache-<zone>, created by the connectivity chart and kept), so the zone is chosen by capacity and brings its own cache; with several zones the pool follows its cache: the slice mounts hf-cache, the first predictor binds it in its node's zone, one of the named, and from then on the predictors run there — a claim Bound in one of the named zones pins the pool to it at once, claims Bound in several of them are refused. Default none: the serving namespace's claims decide — one claim Bound pins the pool to its zone and the slice mounts it; several claims are refused, asking for zones. A re-run moves the pin and the claim.")),
		mcp.WithBoolean(argCache, mcp.DefaultBool(false), mcp.Description("Whether the cluster's serving slice mounts a model cache claim — the zone's claim with zones, else the one claim of the serving namespace. Without the argument the slice keeps its setting where the cluster's slice release runs, and a first slice serves without a cache (default false): a claim is never created unasked; true is the opt-in. The setting is the cluster's — the slice release is the cluster's one, so every pool serves from the same claim — and the claim is a gp3 volume billed every month it exists, after every pool is removed too, until the cache is removed with remove_model_cache: the answer's cache block names the claim, its size, tier and monthly list price in the cluster's region (monthlyPriceUSD, or priceNote why there is none), existing or as the connectivity chart would create it. false composes modelServing.cache.enabled false on the <cluster>-agent-platform release: no claim is applied or mounted, every predictor downloads its weights into its pod's ephemeral storage (the node's local disk) at each start, no zone pin follows from a claim, and the existing claims are left as they are, Helm keeping them and the bill running. Refused (refused{cacheOn{claim, claimName, remedies}}) while the cluster's slice release runs with the cache on — the flip would switch the cache off for the models served on every pool while the claim stays: leave cache on, or remove the cache with remove_model_cache; a first slice with cache false stands, and cache true on a slice that ran without it is the slice's upgrade for every pool. Refused where someone else provides serving — the setting is that layer's.")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: land the objects on the installation as you (the only mode this version offers); commit: a pull request as you (refused until available)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("Render and compare only; nothing is written")),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	), t.createNodePool)

	s.AddTool(mcp.NewTool(ToolDeleteNodePool,
		mcp.WithDescription("Delete a GPU node pool create_node_pool created: removes its idle nodes through their NodeClaims, then its HelmRelease, OCIRepository and values Secret, so helm-controller uninstalls the MachinePool — without waiting for it to be gone: the instance's termination takes minutes, and list_node_pools names the terminating node meanwhile. With the cluster's last pool, the <cluster>-gpu-operator release cluster-manager created, its <cluster>-agent-platform slice release (unless another slice is on in it: sliceKept) and the kserve backend it registered with model-manager go too (lastPool in the answer). The pool's idle nodes go first: the nodes are read on the cluster as you (Karpenter's NodeClaims of the pool, the Nodes registered from them, the pods on each — never the MachinePool's lagging provider IDs while the cluster is readable), a node that no pod with a GPU or KServe predictor holds (a DaemonSet's, finished and preemptible pods do not count) is idle and its NodeClaim is deleted as you — Karpenter drains the node and terminates the instance — listed in objects, and the teardown completes in the same call. Refused while a node of the pool is busy unless force — the message names each busy node with what holds it and the models served on the cluster (a Pending one too) to unload first with model-manager's unload_model — and refused, with no busy node, while a model model-manager serves in the serving namespace has its predictor on no node (Pending, waiting for a node of the pool, or without a pod yet): the message names the model and its pod, since removing the pool would strand it (with the last pool the serving slice, its controller and the backend go too); beside the text a structured refused{nodes, idle, models, unscheduled, hint, readFrom} block, readFrom saying whether the cluster or the MachinePool's provider IDs decided (the latter only when the cluster cannot be read as you). Refused for a pool cluster-manager did not create. dryRun lists what would be removed, the NodeClaims included. The slice goes in order: the llm-d controller's child release (kserve-llmisvc-resources) first — its webhook denies every delete of the well-known LLMInferenceServiceConfigs while it runs —, then, once no controller runs on the cluster, the models model-manager serves in the serving namespace (force took the controller away from under them; their workloads go with them) and the configs of the release namespace, each removed by cluster-manager with its finalizer taken off (nothing else clears it; objects lists them, warnings says so; a serving object made by hand or through GitOps is named and left), then the configs' child release, the operator, the backend registration, the slice release, and the pool's own objects last; so none is left terminating — a served model left behind would sit stopping for good once stopped, the next slice would adopt and lose a config. Answers within the caller's deadline: a step there is no budget left for is pending (partial: true, nextStep names the re-run), and the re-run continues where the teardown stands — the pool's release, removed last, is what it finds. Without force a cluster that cannot be read as you is refused before anything is deleted."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Pool name as given to create_node_pool")),
		mcp.WithBoolean(argForce, mcp.DefaultBool(false), mcp.Description("Delete even while the pool runs nodes: the workloads on them — served models included — are evicted; the well-known LLMInferenceServiceConfigs are removed with their finalizer instead of waiting for the llm-d controller to clear them")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: remove the objects from the installation as you (the only mode this version offers)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("List what would be removed; nothing is written")),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.deleteNodePool)

	s.AddTool(mcp.NewTool(ToolEnableModelServing,
		mcp.WithDescription("Switch model serving on for a cluster, with or without a GPU pool: composes the cluster's one <cluster>-agent-platform release (the agent-platform chart pinned exactly to the version the installation's own platform release runs, at least 4.27.0 — refused below it, naming why; a HelmRelease and its OCIRepository in the cluster's org- namespace, delivered by the installation's Flux) with the serving slice on — KServe and the llm-d control plane with the well-known configs, the nvidia RuntimeClass, the models Gateway at models.<domain> with the login issuer's JWT policy — validating against the platform's Dex service in-cluster on the installation's own cluster (the answer's slice.jwks names the source), against the public issuer on a workload cluster; global.domain, global.identity and where Dex serves its key set (gateway.jwksEgress) read from the platform's own release, never invented; on a workload cluster the target knob (gitops.target.kubeConfig.secretRef: <cluster>-kubeconfig) and agentgateway on, beside the platform's release agentgateway off — and registers the cluster's kserve backend with model-manager. A release that exists is updated in place (never a second release of the chart on one cluster: one under another name is refused, naming it). Refused where the platform's own release or a hand install provides serving already. dryRun returns the rendered manifests and touches nothing. Stranded terminating LLMInferenceServiceConfigs of the release namespace are healed first, as create_node_pool does. cache (default true) is whether the slice's predictors mount the serving namespace's model cache claim, as create_node_pool's: false composes modelServing.cache.enabled false — no claim applied or mounted, the weights in the pod's ephemeral storage, an existing claim left as it is — and the answer says so (cache{enabled, claim, note}); a re-run with a changed value is the slice's upgrade."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name; the installation's own cluster included")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithBoolean(argCache, mcp.DefaultBool(false), mcp.Description("Whether the slice's predictors mount a model cache claim — without the argument the release keeps its setting, and a first slice serves without a cache (default false): a claim is a volume billed every month it exists, never created unasked; true mounts the claim the release mounts already (the zone's claim the last create_node_pool named), else hf-cache; the answer's cache block names its size, tier and monthly list price. false composes modelServing.cache.enabled false on the release — no claim applied or mounted, the weights downloaded into each predictor pod's ephemeral storage, the existing claims left as they are and billed until removed; refused (refused{cacheOn}) while the release runs with the cache on — the way to serve without the cache is remove_model_cache")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: land the objects on the installation as you (the only mode this version offers); commit: a pull request as you (refused until available)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("Render and compare only; nothing is written")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.enableModelServing)

	s.AddTool(mcp.NewTool(ToolDisableModelServing,
		mcp.WithDescription("Switch model serving off for a cluster: removes the <cluster>-agent-platform slice release cluster-manager created (HelmRelease and OCIRepository) and the kserve backend it registered with model-manager, so helm-controller uninstalls KServe and the models Gateway from the cluster. Refused while models are served on the cluster (the message names them) unless force, and plainly when the cluster cannot be read as you; refused for a release cluster-manager did not create. dryRun lists what would be removed. The slice goes in order, as delete_node_pool's last pool does: the llm-d controller's child release first, then — once no controller runs — the models model-manager serves in the serving namespace (force took the controller away from under them) and the well-known LLMInferenceServiceConfigs, each removed with its finalizer taken off (a serving object made by hand or through GitOps is named and left), then the configs' release, the backend registration and the slice release last. Answers within the caller's deadline: what did not fit is pending (partial: true, nextStep) and the re-run continues where the teardown stands."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithBoolean(argForce, mcp.DefaultBool(false), mcp.Description("Remove the serving slice even while models are served: they go with it, and the well-known LLMInferenceServiceConfigs are removed with their finalizer instead of waiting for the llm-d controller to clear them")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: remove the objects from the installation as you (the only mode this version offers)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("List what would be removed; nothing is written")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.disableModelServing)

	s.AddTool(mcp.NewTool(ToolRemoveModelCache,
		mcp.WithDescription("Remove a cluster's model cache: the hf-cache* claims of the serving namespace — gp3 volumes that outlive every pool and the slice release by design and are billed every month they exist, filled or not — deleted on the cluster as you, the volume going with the claim under the class's Delete reclaim policy (a Retain volume is a warning: it stays, and is billed, until deleted by hand). Where cluster-manager's <cluster>-agent-platform slice release runs with the cache on, it is upgraded to modelServing.cache.enabled false first — the connectivity chart applies the claim from a hook on every upgrade, so a claim deleted under a slice with the cache on would come back — and every predictor of the cluster downloads its weights into its pod's ephemeral storage at each start from then on (about 90 s more per cold start); a later create_node_pool or enable_model_serving with cache true creates the claim anew. claim names one claim to remove; default every claim. Refused while a pod of the serving namespace mounts a claim to be removed (refused{models, hint}: the served models to unload first with model-manager's unload_model — a claim a pod mounts is not deleted until the pod is gone), plainly while the cluster cannot be read as you, and where someone else provides serving (the cache is that layer's setting). The answer lists the claims removed with their size, tier and monthly price (removedClaims[]), every claim as read (cacheClaims[]), the slice's objects, and cache.note saying what went and what follows. dryRun lists what would be removed; nothing is written. Answers within the caller's deadline: what did not fit is pending and the re-run continues."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argClaim, mcp.Description("One model cache claim to remove, by name (hf-cache, hf-cache-eu-central-1a); default every hf-cache* claim of the serving namespace. The slice serves without the cache from then on when the claim removed is the one it mounts.")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: remove the objects from the installation and the cluster as you (the only mode this version offers)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("List what would be removed; nothing is written")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.removeModelCache)

	return s
}

func (h *handlers) removeModelCache(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cluster, err := req.RequireString(argCluster)
	if err != nil {
		return errResult(err), nil
	}
	result, err := h.svc.RemoveModelCache(ctx, tools.RemoveModelCacheInput{
		Cluster:   cluster,
		Namespace: req.GetString(argNamespace, ""),
		Claim:     req.GetString(argClaim, ""),
		Mode:      req.GetString(argMode, tools.ModeApply),
		DryRun:    req.GetBool(argDryRun, false),
	})
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(result)
}

func (h *handlers) enableModelServing(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	in, err := modelServingInput(req)
	if err != nil {
		return errResult(err), nil
	}
	result, err := h.svc.EnableModelServing(ctx, in)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(result)
}

func (h *handlers) disableModelServing(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	in, err := modelServingInput(req)
	if err != nil {
		return errResult(err), nil
	}
	result, err := h.svc.DisableModelServing(ctx, in)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(result)
}

// modelServingInput reads the arguments the two model serving tools share.
func modelServingInput(req mcp.CallToolRequest) (tools.ModelServingInput, error) {
	cluster, err := req.RequireString(argCluster)
	if err != nil {
		return tools.ModelServingInput{}, err
	}
	return tools.ModelServingInput{
		Cluster:   cluster,
		Namespace: req.GetString(argNamespace, ""),
		Mode:      req.GetString(argMode, tools.ModeApply),
		DryRun:    req.GetBool(argDryRun, false),
		Force:     req.GetBool(argForce, false),
		Cache:     optionalBool(req, argCache),
	}, nil
}

// optionalBool is a boolean argument as given, nil when the call carries
// none — the tool's default then applies where the argument is decided
// (cache: the slice's setting where the cluster's slice runs, off for a
// first slice).
func optionalBool(req mcp.CallToolRequest, arg string) *bool {
	if _, given := req.GetArguments()[arg]; !given {
		return nil
	}
	v := req.GetBool(arg, true)
	return &v
}

func (h *handlers) createNodePool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cluster, err := req.RequireString(argCluster)
	if err != nil {
		return errResult(err), nil
	}
	name, err := req.RequireString(argName)
	if err != nil {
		return errResult(err), nil
	}
	in := tools.CreateNodePoolInput{
		Cluster:   cluster,
		Namespace: req.GetString(argNamespace, ""),
		Pool: compose.PoolSpec{
			Name:         name,
			Accelerator:  req.GetString(argAccelerator, compose.Accelerators[0]),
			Sizes:        req.GetStringSlice(argSizes, nil),
			MaxGPUs:      req.GetInt(argMaxGPUs, defaultMaxGPUs),
			ChartVersion: req.GetString(argChartVersion, compose.DefaultPoolChartVersion),
			Prewarm:      req.GetBool(argPrewarm, false),
			Zones:        req.GetStringSlice(argZones, nil),
		},
		Cache:  optionalBool(req, argCache),
		Mode:   req.GetString(argMode, tools.ModeApply),
		DryRun: req.GetBool(argDryRun, false),
	}
	result, err := h.svc.CreateNodePool(ctx, in)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(result)
}

func (h *handlers) deleteNodePool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cluster, err := req.RequireString(argCluster)
	if err != nil {
		return errResult(err), nil
	}
	name, err := req.RequireString(argName)
	if err != nil {
		return errResult(err), nil
	}
	result, err := h.svc.DeleteNodePool(ctx, tools.DeleteNodePoolInput{
		Cluster:   cluster,
		Namespace: req.GetString(argNamespace, ""),
		Name:      name,
		Force:     req.GetBool(argForce, false),
		Mode:      req.GetString(argMode, tools.ModeApply),
		DryRun:    req.GetBool(argDryRun, false),
	})
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(result)
}

type handlers struct {
	svc     *tools.Service
	version string
}

func (h *handlers) getInfo(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(Info{Version: h.version, Modes: Modes{Apply: true, Commit: false}, Tools: ToolNames(), ClusterAPI: h.svc.ClusterAPI(ctx)})
}

func (h *handlers) listClusters(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	clusters, err := h.svc.ListClusters(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(clusters)
}

func (h *handlers) listNodePools(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cluster, err := req.RequireString(argCluster)
	if err != nil {
		return errResult(err), nil
	}
	pools, err := h.svc.ListNodePools(ctx, cluster, req.GetString(argNamespace, ""))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(pools)
}

// jsonResult renders v as the tool's text content.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return mcp.NewToolResultText(string(b)), nil
}

// errResult is a tool error: the message names the cause so the caller can
// act on it (a missing cluster, an ambiguous name, a refused read).
func errResult(err error) *mcp.CallToolResult {
	var refused *tools.ErrRefused
	if errors.As(err, &refused) && refused.Refused != nil {
		// The structured refusal beside the text: a second text content
		// carrying {"refused": {nodes, models, hint}}.
		block, _ := json.Marshal(map[string]any{"refused": refused.Refused})
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.NewTextContent(err.Error()), mcp.NewTextContent(string(block))}}
	}
	var notFound *tools.ErrNotFound
	if errors.As(err, &notFound) {
		return mcp.NewToolResultError(err.Error() + ": list_clusters names the clusters you may see")
	}
	return mcp.NewToolResultError(err.Error())
}
