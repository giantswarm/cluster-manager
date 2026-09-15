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
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{ToolGetInfo, ToolListClusters, ToolListNodePools, ToolCreateNodePool, ToolDeleteNodePool}
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
		mcpserver.WithInstructions("Manage the clusters of this Giant Swarm installation and their GPU node pools. list_clusters names every cluster with its organization, release, whether it is the installation's own cluster, whether the GPU operator and the serving layer are present and who provides them, and its GPU pool releases; list_node_pools shows one cluster's MachinePools with the pool's Kubernetes version and the control plane's side by side; create_node_pool composes a GPU pool's release (gpu-node-pool chart) from the cluster's current release and settings and lands it as you — a second call on the same name is the update, dryRun the drift check; delete_node_pool removes what create_node_pool created and refuses while the pool runs nodes. Every call runs as you: what you may read is what these tools list, what you may write is what they change."),
	)
	t := &handlers{svc: svc, version: version}

	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Report this server's version, the write modes it offers (apply: objects landed on the installation as the caller; commit: a pull request opened as the caller) and the names of its tools."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListClusters,
		mcp.WithDescription("List the installation's clusters: name, organization and namespace, Giant Swarm release version, whether the cluster is the installation's own (its management cluster), whether the GPU operator and the serving layer are present and who provides them (chart, cluster-manager, manual; unknown until detection runs), the GPU pool releases (HelmReleases of the gpu-node-pool chart) and the commit target (the git repository and path owning the cluster, null when none). Nothing the portal's Clusters pages already show."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listClusters)

	s.AddTool(mcp.NewTool(ToolListNodePools,
		mcp.WithDescription("List the MachinePools of one cluster with the pool's Kubernetes version and the control plane's as two fields (no verdict is drawn), replicas and ready replicas, the instance types when readable, the accelerator of the owning pool release, and the HelmRelease that owns the pool (null for a pool created by other means)."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listNodePools)

	s.AddTool(mcp.NewTool(ToolCreateNodePool,
		mcp.WithDescription("Create a GPU node pool for a cluster, or update the pool of that name: composes the pool's release of the gpu-node-pool chart (a HelmRelease and its OCIRepository in the cluster's org- namespace) with the Kubernetes version and Flatcar machine image of the cluster's current release — refused when the release runs ahead of the control plane — and a credential-free snapshot of the cluster's settings (registry mirrors, proxy, base domain, management cluster, Cilium IPAM mode; registry credentials go into a valuesFrom Secret). Mode apply lands the objects on the installation as you, owned by the Cluster so they go with it; a second call on the same name is the update, and its dryRun shows the difference. dryRun returns the rendered manifests and touches nothing. An object of that name someone else owns (GitOps) is never patched."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argName, mcp.Required(), mcp.Pattern(compose.PoolNamePattern.String()), mcp.Description("Pool name within the cluster, five to twenty lowercase characters, digits and dashes (gpu00, gpu-l4); <cluster>-<name> names the MachinePool")),
		mcp.WithString(argAccelerator, mcp.Enum(compose.Accelerators...), mcp.DefaultString(compose.Accelerators[0]), mcp.Description("Accelerator from the curated list; picks the EC2 instance family")),
		mcp.WithArray(argSizes, mcp.Items(map[string]any{"type": "string"}), mcp.Description("Instance sizes Karpenter may pick within the family, smallest first (xlarge, 2xlarge, ...); default the chart's")),
		mcp.WithNumber(argMaxGPUs, mcp.Min(1), mcp.DefaultNumber(defaultMaxGPUs), mcp.Description("Upper bound of the pool in GPUs across all of its nodes; the minimum is always 0 (scale to zero)")),
		mcp.WithString(argChartVersion, mcp.DefaultString(compose.DefaultPoolChartVersion), mcp.Description("Exact gpu-node-pool chart version to pin; default the newest released")),
		mcp.WithBoolean(argTeleport, mcp.Description("Join the nodes to Teleport; default on when the cluster has its teleport join-token Secret, off otherwise")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: land the objects on the installation as you (the only mode this version offers); commit: a pull request as you (refused until available)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("Render and compare only; nothing is written")),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	), t.createNodePool)

	s.AddTool(mcp.NewTool(ToolDeleteNodePool,
		mcp.WithDescription("Delete a GPU node pool create_node_pool created: removes its HelmRelease, OCIRepository and values Secret, so helm-controller uninstalls the MachinePool and its nodes. Refused while the pool still runs nodes (the message names them) unless force; refused for a pool cluster-manager did not create. dryRun lists what would be removed."),
		mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name")),
		mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation")),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Pool name as given to create_node_pool")),
		mcp.WithBoolean(argForce, mcp.DefaultBool(false), mcp.Description("Delete even while the pool runs nodes: the workloads on them are evicted")),
		mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description("apply: remove the objects from the installation as you (the only mode this version offers)")),
		mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description("List what would be removed; nothing is written")),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.deleteNodePool)

	return s
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

func (h *handlers) getInfo(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(Info{Version: h.version, Modes: Modes{Apply: true, Commit: false}, Tools: ToolNames()})
}

func (h *handlers) listClusters(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	clusters, err := h.svc.ListClusters(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"clusters": clusters})
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
