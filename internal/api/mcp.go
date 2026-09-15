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

	"github.com/giantswarm/cluster-manager/internal/tools"
)

// MCP tool names. Through muster they appear as x_<server>_<tool>, e.g.
// x_cluster-manager_list_clusters.
const (
	ToolGetInfo       = "get_info"
	ToolListClusters  = "list_clusters"
	ToolListNodePools = "list_node_pools"
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{ToolGetInfo, ToolListClusters, ToolListNodePools}
}

const (
	argCluster   = "cluster"
	argNamespace = "namespace"
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
		mcpserver.WithInstructions("Manage the clusters of this Giant Swarm installation and their GPU node pools. list_clusters names every cluster with its organization, release, whether it is the installation's own cluster, whether the GPU operator and the serving layer are present and who provides them, and its GPU pool releases; list_node_pools shows one cluster's MachinePools with the pool's Kubernetes version and the control plane's side by side. Every call runs as you: what you may read is what these tools list."),
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

	return s
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
