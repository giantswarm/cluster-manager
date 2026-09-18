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
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{ToolGetInfo, ToolListClusters, ToolListNodePools, ToolCreateNodePool, ToolDeleteNodePool, ToolEnableModelServing, ToolDisableModelServing}
}

const (
	argCluster      = "cluster"
	argNamespace    = "namespace"
	argName         = "name"
	argAccelerator  = "accelerator"
	argSizes        = "sizes"
	argMaxGPUs      = "maxGpus"
	argChartVersion = "chartVersion"
	argTeleport     = "teleport"
	argForce        = "force"
	argMode         = "mode"
	argDryRun       = "dryRun"
	argPrewarm      = "prewarm"
	argZones        = "zones"
	argCache        = "cache"

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
		mcpserver.WithInstructions("Manage the clusters of this Giant Swarm installation and their GPU node pools. list_clusters names every cluster with its organization, release, whether it is the installation's own cluster, whether the GPU operator and the serving layer are present and who provides them, and its GPU pool releases; list_node_pools shows one cluster's MachinePools with the pool's Kubernetes version and the control plane's side by side; create_node_pool composes a GPU pool's release (gpu-node-pool chart) from the cluster's current release and settings and lands it as you — a second call on the same name is the update, dryRun the drift check; delete_node_pool removes what create_node_pool created, the pool's idle nodes first, and refuses while a node of the pool is busy; enable_model_serving and disable_model_serving switch the serving slice (KServe, the models Gateway) on or off for a cluster through its one <cluster>-agent-platform release, with or without a GPU pool. Every call runs as you: what you may read is what these tools list, what you may write is what they change."),
	)
	t := &handlers{svc: svc, version: version}

	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Report this server's version, the write modes it offers (apply: objects landed on the installation as the caller; commit: a pull request opened as the caller), the names of its tools, and whether the installation serves the Cluster API (cluster.x-k8s.io) the cluster tools read from — absent on an installation without the KaaS components, where list_clusters is empty and the tools naming a cluster refuse."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListClusters,
		mcp.WithDescription("List the installation's clusters with the GPU operator's and the serving layer's readiness as structured fields (gpuOperator.readiness: the operator release's Ready condition, the ClusterPolicy's state, the device plugin and GPU feature discovery DaemonSets' scheduled and ready pods — 0/0 at scale-to-zero; serving.readiness: the slice release and every child release with its Ready condition, the KServe controllers' available replicas, the LLMInferenceServiceConfigs count, the kserve backend registered with model-manager, the published presets count, the models Gateway's Programmed condition, the model cache claim of the serving namespace with the zone its volume is bound to — where every GPU pool created while the claim exists lands): name, organization and namespace, Giant Swarm release version, whether the cluster is the installation's own (its management cluster), whether the GPU operator and the serving layer are present and who provides them — chart (the platform's own release), cluster-manager, manual (by hand) — with the evidence, or unknown with the reason when the cluster cannot be read as you (serving is present with a KServe controller on the cluster, never with the KServe CRDs alone, which Helm leaves behind when a serving layer goes: those are absent with the served APIs noted); the GPU pool releases (HelmReleases of the gpu-node-pool chart) and the commit target (the git repository and path owning the cluster, null when none). Nothing the portal's Clusters pages already show. On an installation that does not serve the Cluster API (cluster.x-k8s.io) the list is empty and clusterApi says so."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listClusters)

	s.AddTool(mcp.NewTool(ToolListNodePools,
		mcp.WithDescription("List the pools of one cluster with their lifecycle: phase (creating, ready, scaling, removing, failed) and steps — the pool release Ready, the MachinePool ready, the nodes (NodeClaims launching, ready, terminating — a terminating node named with its deletion time while Karpenter drains it and terminates its instance; a node Karpenter could not launch named with its refusal — every size refused, the zones tried, the zones AWS named as having the capacity when Karpenter's message still carries them, Karpenter's words verbatim — and the way around it: for a pool pinned to zones, what pinned it — the model cache claim's zone, or the zones named on create — and the re-run of create_node_pool that moves it (zones naming a zone with capacity, with cache false while the claim pins), else wider sizes or another accelerator; on a GPU pool of cluster-manager's the ready nodes that hold nothing named idle since their last pod left, which delete_node_pool removes with the pool, and a MachinePool still listing instances the cluster no longer has said so), and for a pool created with prewarm the placeholder (prewarm: pending until the pool release installs its Job, then the Job's pod pending while the first node launches, holding a node, preempted by the first workload, finished when the hold ended, or absent after its TTL — the step never decides the phase) — each pending, inProgress, done or failed with since/finishedAt timestamps; a pool release whose MachinePool is not created yet is listed creating, a pool under delete_node_pool stays listed removing with deleting: true and the teardown's pending objects until its HelmRelease is gone, and by its MachinePool, removing with the terminating node named, until the NodeClaim has gone with the terminated instance; plus the pool's Kubernetes version and the control plane's as two fields (no verdict is drawn), replicas and ready replicas, the instance types when readable, the accelerator of the owning pool release and its sizes (sizes: each size as AWS lists it, what it leaves a predictor, and its on-demand price per hour in the cluster's region — pricePerHourUSD, priceSource, priceAsOf, or priceNote saying why there is none), and the HelmRelease that owns the pool (null for a pool created by other means)."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listNodePools)

	s.AddTool(mcp.NewTool(ToolCreateNodePool,
		mcp.WithDescription("Create a GPU node pool for a cluster, or update the pool of that name: composes the pool's release of the gpu-node-pool chart (a HelmRelease and its OCIRepository in the cluster's org- namespace) with the Kubernetes version and Flatcar machine image of the cluster's current release — refused when the release runs ahead of the control plane — and a credential-free snapshot of the cluster's settings (registry mirrors, proxy, base domain, management cluster, Cilium IPAM mode; registry credentials go into a valuesFrom Secret). Where no GPU operator runs it composes the <cluster>-gpu-operator release; where nothing provides serving it composes the cluster's <cluster>-agent-platform release with the serving slice on (KServe, the nvidia RuntimeClass, the models Gateway at models.<domain>; domain, identity and the chart version read from the platform's own release), updated in place on a re-run — the re-run moves the pin to the version the platform runs today; it registers the cluster's kserve backend with model-manager. Mode apply lands the objects on the installation as you, owned by the Cluster so they go with it; a second call on the same name is the update, and its dryRun shows the difference. dryRun returns the rendered manifests and touches nothing. An object of that name someone else owns (GitOps) is never patched. The answer lists the pool's sizes (sizes) with what each leaves a predictor after the node's kubelet reservations and daemonsets and its on-demand price per hour in the cluster's region (pricePerHourUSD with priceSource and priceAsOf, from AWS's on-demand Linux list price; a region the price table does not cover, or a size not offered there, carries priceNote instead — never a guess), and the smallest size that hosts each serving preset (presetFit): the presets published on the cluster where a serving layer publishes them (presetFit.origin published), else the ones the slice would publish — the connectivity chart's shipped set at the version the slice's agent-platform release resolves, read from the registry (origin chart; presetFit.source names the chart and version) — so a first pool's size can be chosen by model before the slice exists; each preset carries its displayName and model id; a preset the accelerator could serve but no size of the pool hosts is a warning (warnings) — its predictor would sit Pending while Karpenter refuses every size. With prewarm the installation's own pool launches its first node with the release (a placeholder Job holding one GPU until the first workload preempts it), so a model named up front is served minutes earlier; refused for a workload cluster's pool. Where the pool runs is the person's: zones pins the pool's nodes to the named availability zones (the chart's pool.zones — placeholder and predictors alike come up there), checked against the cluster's node subnets (a zone the cluster has no node subnet in is refused, naming the zones it has) and against the serving namespace's model cache claim — one volume, bound in one zone by the first predictor that mounted it, kept when the pool goes: a claim Bound in a zone outside the named ones is refused, since a node elsewhere strands every predictor mounting the cache Pending — beside the text a structured refused{cacheZone{claim, claimZone, zones, remedies}} block, the ways out being to name the claim's zone, to pass cache false, or to remove the claim; a claim inside them pins nothing further. Without zones the pool follows the model cache: with the claim Bound the nodes are pinned to its zone; no claim, no pin; a claim that is not Bound or names no zone pins nothing and zonesNote says so; a claim not readable as you pins nothing and is a warning. The answer names the pin and whose it is (zones, zonesNote, cacheClaim); dryRun shows it. cache (default true) is whether this pool's serving slice mounts the claim at all: false composes modelServing.cache.enabled false on the <cluster>-agent-platform release — no claim is applied or mounted, every predictor downloads its weights into its pod's ephemeral storage (the node's local disk), no zone follows from a claim, and an existing claim is left as it is (Helm keeps it) — and the answer says so (cache{enabled, claim, note}); a re-run with a changed value is the slice's upgrade; refused where someone else provides serving. A family not offered in a pinned zone fails its launch, named in list_node_pools' nodes step with what pinned the pool. LLMInferenceServiceConfigs a serving layer that went left terminating in the release namespace are removed first, finalizer and all, when no llm-d controller runs (objects lists them, warnings says why), so the slice's release creates them afresh instead of adopting and losing them."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argName, mcp.Required(), mcp.Pattern(compose.PoolNamePattern.String()), mcp.Description("Pool name within the cluster, five to twenty lowercase characters, digits and dashes (gpu00, gpu-l4); <cluster>-<name> names the MachinePool")),
		mcp.WithString(argAccelerator, mcp.Enum(compose.Accelerators...), mcp.DefaultString(compose.Accelerators[0]), mcp.Description("Accelerator from the curated list; picks the EC2 instance family")),
		mcp.WithArray(argSizes, mcp.Items(map[string]any{"type": "string"}), mcp.Description("Instance sizes Karpenter may pick within the family, smallest first (xlarge, 2xlarge, ...; a size the family does not have is refused); default the chart's: xlarge, 2xlarge, 4xlarge")),
		mcp.WithNumber(argMaxGPUs, mcp.Min(1), mcp.DefaultNumber(defaultMaxGPUs), mcp.Description("Upper bound of the pool in GPUs across all of its nodes; the minimum is always 0 (scale to zero)")),
		mcp.WithString(argChartVersion, mcp.DefaultString(compose.DefaultPoolChartVersion), mcp.Description("Exact gpu-node-pool chart version to pin; default the newest released")),
		mcp.WithBoolean(argTeleport, mcp.Description("Join the nodes to Teleport; default on when the cluster has its teleport join-token Secret, off otherwise")),
		mcp.WithBoolean(argPrewarm, mcp.DefaultBool(false), mcp.Description("Launch the pool's first node with the release instead of with the first predictor (pool.prewarm.enabled): a one-shot placeholder Job in the release namespace holds one GPU at negative priority until the first workload preempts it or its hold (the chart's default, 15 minutes) ends and Karpenter consolidates the empty node. Only for the installation's own pool — the Job runs on the installation, where only that pool's nodes join; refused for a workload cluster's pool, naming why. The Job is created with the release's install only: a re-run on an existing pool flips the value but never launches a placeholder again, and a re-run with prewarm false removes the block. list_node_pools shows the placeholder as the prewarm step.")),
		mcp.WithArray(argZones, mcp.Items(map[string]any{"type": "string"}), mcp.Description("Availability zones the pool's nodes are pinned to (pool.zones; eu-central-1a), among the zones of the cluster's node subnets — a zone the cluster has no node subnet in is refused, naming the zones it has. Judged against the serving namespace's model cache claim: a claim Bound in a zone outside them is refused, naming the claim's zone and the ways out (name it among the zones, pass cache false, remove the claim); a claim inside them pins nothing further. Default none: the pool follows the claim's zone when it is Bound, else nothing is pinned. A re-run moves the pin.")),
		mcp.WithBoolean(argCache, mcp.DefaultBool(true), mcp.Description("Whether this pool's serving slice mounts the model cache claim (default true). false composes modelServing.cache.enabled false on the <cluster>-agent-platform release: no claim is applied or mounted, every predictor downloads its weights into its pod's ephemeral storage (the node's local disk) at each start, no zone pin follows from a claim, and an existing claim is left as it is (Helm keeps it). A re-run with a changed value is the slice's upgrade. Refused where someone else provides serving — the setting is that layer's.")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: land the objects on the installation as you (the only mode this version offers); commit: a pull request as you (refused until available)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("Render and compare only; nothing is written")),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	), t.createNodePool)

	s.AddTool(mcp.NewTool(ToolDeleteNodePool,
		mcp.WithDescription("Delete a GPU node pool create_node_pool created: removes its idle nodes through their NodeClaims, then its HelmRelease, OCIRepository and values Secret, so helm-controller uninstalls the MachinePool — without waiting for it to be gone: the instance's termination takes minutes, and list_node_pools names the terminating node meanwhile. With the cluster's last pool, the <cluster>-gpu-operator release cluster-manager created, its <cluster>-agent-platform slice release (unless another slice is on in it: sliceKept) and the kserve backend it registered with model-manager go too (lastPool in the answer). The pool's idle nodes go first: the nodes are read on the cluster as you (Karpenter's NodeClaims of the pool, the Nodes registered from them, the pods on each — never the MachinePool's lagging provider IDs while the cluster is readable), a node that no pod with a GPU or KServe predictor holds (a DaemonSet's, finished and preemptible pods do not count) is idle and its NodeClaim is deleted as you — Karpenter drains the node and terminates the instance — listed in objects, and the teardown completes in the same call. Refused while a node of the pool is busy unless force — the message names each busy node with what holds it and the models served on the cluster (a Pending one too) to unload first with model-manager's unload_model — and refused, with no busy node, while a model model-manager serves in the serving namespace has its predictor on no node (Pending, waiting for a node of the pool, or without a pod yet): the message names the model and its pod, since removing the pool would strand it (with the last pool the serving slice, its controller and the backend go too, and the serving object is left behind with a finalizer nothing clears); beside the text a structured refused{nodes, idle, models, unscheduled, hint, readFrom} block, readFrom saying whether the cluster or the MachinePool's provider IDs decided (the latter only when the cluster cannot be read as you). Refused for a pool cluster-manager did not create. dryRun lists what would be removed, the NodeClaims included. The slice goes in order: the llm-d controller's child release (kserve-llmisvc-resources) first — its webhook denies every delete of the well-known LLMInferenceServiceConfigs while it runs —, then the configs of the release namespace, removed by cluster-manager with their finalizer taken off once no controller runs on the cluster (nothing else clears it; warnings says so), then the configs' child release, the operator, the backend registration, the slice release, and the pool's own objects last; so none is left terminating for the next slice to adopt and lose. Answers within the caller's deadline: a step there is no budget left for is pending (partial: true, nextStep names the re-run), and the re-run continues where the teardown stands — the pool's release, removed last, is what it finds. Without force a cluster that cannot be read as you is refused before anything is deleted."),
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
		mcp.WithBoolean(argCache, mcp.DefaultBool(true), mcp.Description("Whether the slice's predictors mount the model cache claim (default true); false composes modelServing.cache.enabled false on the release — no claim applied or mounted, the weights downloaded into each predictor pod's ephemeral storage, an existing claim left as it is")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: land the objects on the installation as you (the only mode this version offers); commit: a pull request as you (refused until available)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("Render and compare only; nothing is written")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.enableModelServing)

	s.AddTool(mcp.NewTool(ToolDisableModelServing,
		mcp.WithDescription("Switch model serving off for a cluster: removes the <cluster>-agent-platform slice release cluster-manager created (HelmRelease and OCIRepository) and the kserve backend it registered with model-manager, so helm-controller uninstalls KServe and the models Gateway from the cluster. Refused while models are served on the cluster (the message names them) unless force, and plainly when the cluster cannot be read as you; refused for a release cluster-manager did not create. dryRun lists what would be removed. The slice goes in order, as delete_node_pool's last pool does: the llm-d controller's child release first, then its well-known LLMInferenceServiceConfigs (removed with their finalizer once no controller runs to deny the delete), then the configs' release, the backend registration and the slice release last. Answers within the caller's deadline: what did not fit is pending (partial: true, nextStep) and the re-run continues where the teardown stands."),
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

	return s
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
// (teleport by the cluster's join-token Secret, cache on).
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
		Teleport: optionalBool(req, argTeleport),
		Cache:    optionalBool(req, argCache),
		Mode:     req.GetString(argMode, tools.ModeApply),
		DryRun:   req.GetBool(argDryRun, false),
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
