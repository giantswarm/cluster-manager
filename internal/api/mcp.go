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
		mcpserver.WithInstructions("Manage the clusters of this Giant Swarm installation and their GPU node pools. list_clusters names every cluster with its organization, release, whether it is the installation's own cluster, whether the GPU operator and the serving layer are present and who provides them, and its GPU pool releases; list_node_pools shows one cluster's MachinePools with the pool's Kubernetes version and the control plane's side by side; create_node_pool composes a GPU pool's release (gpu-node-pool chart) from the cluster's current release and settings and lands it as you — a second call on the same name is the update, dryRun the drift check; delete_node_pool removes what create_node_pool created and refuses while the pool runs nodes; enable_model_serving and disable_model_serving switch the serving slice (KServe, the models Gateway) on or off for a cluster through its one <cluster>-agent-platform release, with or without a GPU pool. Every call runs as you: what you may read is what these tools list, what you may write is what they change."),
	)
	t := &handlers{svc: svc, version: version}

	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Report this server's version, the write modes it offers (apply: objects landed on the installation as the caller; commit: a pull request opened as the caller), the names of its tools, and whether the installation serves the Cluster API (cluster.x-k8s.io) the cluster tools read from — absent on an installation without the KaaS components, where list_clusters is empty and the tools naming a cluster refuse."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListClusters,
		mcp.WithDescription("List the installation's clusters: name, organization and namespace, Giant Swarm release version, whether the cluster is the installation's own (its management cluster), whether the GPU operator and the serving layer are present and who provides them — chart (the platform's own release), cluster-manager, manual (by hand) — with the evidence, or unknown with the reason when the cluster cannot be read as you (serving is present with a KServe controller on the cluster, never with the KServe CRDs alone, which Helm leaves behind when a serving layer goes: those are absent with the served APIs noted); the GPU pool releases (HelmReleases of the gpu-node-pool chart) and the commit target (the git repository and path owning the cluster, null when none). Nothing the portal's Clusters pages already show. On an installation that does not serve the Cluster API (cluster.x-k8s.io) the list is empty and clusterApi says so."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listClusters)

	s.AddTool(mcp.NewTool(ToolListNodePools,
		mcp.WithDescription("List the MachinePools of one cluster with the pool's Kubernetes version and the control plane's as two fields (no verdict is drawn), replicas and ready replicas, the instance types when readable, the accelerator of the owning pool release, and the HelmRelease that owns the pool (null for a pool created by other means)."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listNodePools)

	s.AddTool(mcp.NewTool(ToolCreateNodePool,
		mcp.WithDescription("Create a GPU node pool for a cluster, or update the pool of that name: composes the pool's release of the gpu-node-pool chart (a HelmRelease and its OCIRepository in the cluster's org- namespace) with the Kubernetes version and Flatcar machine image of the cluster's current release — refused when the release runs ahead of the control plane — and a credential-free snapshot of the cluster's settings (registry mirrors, proxy, base domain, management cluster, Cilium IPAM mode; registry credentials go into a valuesFrom Secret). Where no GPU operator runs it composes the <cluster>-gpu-operator release; where nothing provides serving it composes the cluster's <cluster>-agent-platform release with the serving slice on (KServe, the nvidia RuntimeClass, the models Gateway at models.<domain>; domain, identity and the chart version read from the platform's own release), updated in place on a re-run — the re-run moves the pin to the version the platform runs today; it registers the cluster's kserve backend with model-manager. Mode apply lands the objects on the installation as you, owned by the Cluster so they go with it; a second call on the same name is the update, and its dryRun shows the difference. dryRun returns the rendered manifests and touches nothing. An object of that name someone else owns (GitOps) is never patched. The answer lists the pool's sizes with what each leaves a predictor after the node's kubelet reservations and daemonsets (sizes) and, where the cluster publishes serving presets, the smallest size that hosts each preset (presetFit); a preset the accelerator could serve but no size of the pool hosts is a warning (warnings) — its predictor would sit Pending while Karpenter refuses every size. LLMInferenceServiceConfigs a serving layer that went left terminating in the release namespace are removed first, finalizer and all, when no llm-d controller runs (objects lists them, warnings says why), so the slice's release creates them afresh instead of adopting and losing them."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argName, mcp.Required(), mcp.Pattern(compose.PoolNamePattern.String()), mcp.Description("Pool name within the cluster, five to twenty lowercase characters, digits and dashes (gpu00, gpu-l4); <cluster>-<name> names the MachinePool")),
		mcp.WithString(argAccelerator, mcp.Enum(compose.Accelerators...), mcp.DefaultString(compose.Accelerators[0]), mcp.Description("Accelerator from the curated list; picks the EC2 instance family")),
		mcp.WithArray(argSizes, mcp.Items(map[string]any{"type": "string"}), mcp.Description("Instance sizes Karpenter may pick within the family, smallest first (xlarge, 2xlarge, ...; a size the family does not have is refused); default the chart's: xlarge, 2xlarge, 4xlarge")),
		mcp.WithNumber(argMaxGPUs, mcp.Min(1), mcp.DefaultNumber(defaultMaxGPUs), mcp.Description("Upper bound of the pool in GPUs across all of its nodes; the minimum is always 0 (scale to zero)")),
		mcp.WithString(argChartVersion, mcp.DefaultString(compose.DefaultPoolChartVersion), mcp.Description("Exact gpu-node-pool chart version to pin; default the newest released")),
		mcp.WithBoolean(argTeleport, mcp.Description("Join the nodes to Teleport; default on when the cluster has its teleport join-token Secret, off otherwise")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: land the objects on the installation as you (the only mode this version offers); commit: a pull request as you (refused until available)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("Render and compare only; nothing is written")),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	), t.createNodePool)

	s.AddTool(mcp.NewTool(ToolDeleteNodePool,
		mcp.WithDescription("Delete a GPU node pool create_node_pool created: removes its HelmRelease, OCIRepository and values Secret, so helm-controller uninstalls the MachinePool and its nodes. With the cluster's last pool, the <cluster>-gpu-operator release cluster-manager created, its <cluster>-agent-platform slice release (unless another slice is on in it: sliceKept) and the kserve backend it registered with model-manager go too (lastPool in the answer). Refused while the pool still runs nodes unless force — the message names the nodes and the models served on the cluster (a Pending one too) to unload first with model-manager's unload_model, or says that none is served and something else holds the nodes; refused for a pool cluster-manager did not create. dryRun lists what would be removed. The slice goes in order: its kserve-runtime-configs child release first, then every LLMInferenceServiceConfig of the release namespace is seen gone from the cluster — waited for while the llm-d controller runs to clear them, removed with their finalizer where nothing else will (the controller gone, force, or two minutes over; warnings says which) — then the rest, so none is left terminating for the next slice to adopt and lose. Without force a cluster that cannot be read as you is refused before anything is deleted."),
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
		mcp.WithDescription("Switch model serving on for a cluster, with or without a GPU pool: composes the cluster's one <cluster>-agent-platform release (the agent-platform chart pinned exactly to the version the installation's own platform release runs, at least 4.27.0 — refused below it, naming why; a HelmRelease and its OCIRepository in the cluster's org- namespace, delivered by the installation's Flux) with the serving slice on — KServe and the llm-d control plane with the well-known configs, the nvidia RuntimeClass, the models Gateway at models.<domain> with the login issuer's JWT policy — validating against the platform's Dex service in-cluster on the installation's own cluster (the answer's slice.jwks names the source), against the public issuer on a workload cluster; global.domain, global.identity and where Dex serves its key set (gateway.jwksEgress) read from the platform's own release, never invented; on a workload cluster the target knob (gitops.target.kubeConfig.secretRef: <cluster>-kubeconfig) and agentgateway on, beside the platform's release agentgateway off — and registers the cluster's kserve backend with model-manager. A release that exists is updated in place (never a second release of the chart on one cluster: one under another name is refused, naming it). Refused where the platform's own release or a hand install provides serving already. dryRun returns the rendered manifests and touches nothing. Stranded terminating LLMInferenceServiceConfigs of the release namespace are healed first, as create_node_pool does."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name; the installation's own cluster included")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: land the objects on the installation as you (the only mode this version offers); commit: a pull request as you (refused until available)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("Render and compare only; nothing is written")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.enableModelServing)

	s.AddTool(mcp.NewTool(ToolDisableModelServing,
		mcp.WithDescription("Switch model serving off for a cluster: removes the <cluster>-agent-platform slice release cluster-manager created (HelmRelease and OCIRepository) and the kserve backend it registered with model-manager, so helm-controller uninstalls KServe and the models Gateway from the cluster. Refused while models are served on the cluster (the message names them) unless force, and plainly when the cluster cannot be read as you; refused for a release cluster-manager did not create. dryRun lists what would be removed. The slice goes in order, as delete_node_pool's last pool does: the kserve-runtime-configs child release first, the LLMInferenceServiceConfigs seen gone (waited for, or removed with their finalizer where nothing else will), then the rest."),
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
	}, nil
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
		},
		Mode:   req.GetString(argMode, tools.ModeApply),
		DryRun: req.GetBool(argDryRun, false),
	}
	if _, given := req.GetArguments()[argTeleport]; given {
		teleport := req.GetBool(argTeleport, true)
		in.Teleport = &teleport
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
	var notFound *tools.ErrNotFound
	if errors.As(err, &notFound) {
		return mcp.NewToolResultError(err.Error() + ": list_clusters names the clusters you may see")
	}
	return mcp.NewToolResultError(err.Error())
}
