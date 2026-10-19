// Package api registers cluster-manager's MCP tools. MCP is the only surface:
// agents call the tools through muster, and so does the Dev Portal, as the
// signed-in person.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
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
	ToolListReleases   = "list_releases"
	ToolCreateCluster  = "create_cluster"
	ToolDeleteCluster  = "delete_cluster"

	ToolEnableModelServing  = "enable_model_serving"
	ToolDisableModelServing = "disable_model_serving"
	ToolRemoveModelCache    = "remove_model_cache"
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{ToolGetInfo, ToolListClusters, ToolListReleases, ToolCreateCluster, ToolDeleteCluster, ToolListNodePools, ToolCreateNodePool, ToolDeleteNodePool, ToolEnableModelServing, ToolDisableModelServing, ToolRemoveModelCache}
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
	argServing      = "serving"
	argClaim        = "claim"
	argOrganization = "organization"
	argProvider     = "provider"
	argRelease      = "release"
	argIdentity     = "identity"
	argDescription  = "description"
	argValues       = "values"

	defaultMaxGPUs = 4
)

// Info is get_info's answer.
type Info struct {
	Version string `json:"version"`
	// Modes are the write modes this server offers: apply lands objects on
	// the installation as the caller; commit opens a pull request as the
	// caller, offered when the server is registered with its GitHub App.
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
	// CommitTools are the tools that take mode commit.
	CommitTools []string `json:"commitTools"`
}

// commitTools are the tools that take mode commit; every other write lands
// in mode apply only.
var commitTools = []string{ToolCreateCluster, ToolDeleteCluster, ToolCreateNodePool, ToolDeleteNodePool}

// NewMCPServer builds the MCP server exposing the tools. Results are JSON
// text.
func NewMCPServer(svc *tools.Service, version string) *mcpserver.MCPServer {
	opts := append(tracingOptions(),
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the clusters of this Giant Swarm installation and their GPU node pools. list_clusters names every cluster with its organization, release, whether it is the installation's own cluster, whether the GPU operator and the serving layer are present and who provides them, and its GPU pool releases; list_releases names the installation's releases with the ones a new cluster may run; create_cluster composes a workload cluster (the release chart's HelmRelease, its OCIRepository and a values ConfigMap in the organization's namespace), checked against the release chart's schema, and lands it as you or, in mode commit, opens the pull request that adds it to the repository owning the organization; delete_cluster removes a cluster create_cluster created, as you, its GPU pools with it, and refuses the installation's own cluster and one Flux applies from git; list_node_pools shows one cluster's MachinePools with the pool's Kubernetes version and the control plane's side by side; create_node_pool composes a GPU pool's release (gpu-node-pool chart) from the cluster's current release and settings and lands it as you — a second call on the same name is the update, dryRun the drift check; delete_node_pool removes what create_node_pool created, the pool's idle nodes first, and refuses while a node of the pool is busy; enable_model_serving and disable_model_serving switch the serving slice (KServe, the models Gateway) on or off for a cluster through its one <cluster>-agent-platform release, with or without a GPU pool; remove_model_cache removes a cluster's model cache — the claims that outlive every pool and are billed while they exist — after switching the slice to serve without it. Every call runs as you: what you may read is what these tools list, what you may write is what they change."),
	)
	s := mcpserver.NewMCPServer("cluster-manager", version, opts...)
	t := &handlers{svc: svc, version: version}

	s.AddTool(mcp.NewTool(ToolGetInfo,
		tooldesc.Describe("Report this server's version, write modes (apply, commit), tools, and whether the installation serves the Cluster API.",
			"apply lands objects on the installation as you; commit opens a pull request as you. "+
				"Without the Cluster API (cluster.x-k8s.io), list_clusters is empty and the tools naming a cluster refuse."),
		readOnly(),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolListClusters,
		tooldesc.Describe("List the installation's clusters with their organization, release, GPU operator, model serving, zones and model cache.",
			"Per cluster: whether it is the installation's own; who provides the GPU operator and the serving layer (chart, cluster-manager, manual or unknown) and their readiness; "+
				"the zones of its node subnets a pool may be pinned to; its GPU pool releases; where commit mode writes (commitTarget); "+
				"its model cache claims with their zone and monthly price; and for a cluster create_cluster made, who it binds as cluster-admin. "+
				"Empty where the installation does not serve the Cluster API."),
		readOnly(),
	), t.listClusters)

	s.AddTool(mcp.NewTool(ToolListReleases,
		tooldesc.Describe("List the installation's Giant Swarm releases, newest first per provider line, and whether create_cluster offers each.",
			"Each with its state (active, deprecated, wip, preview), Kubernetes version, cluster chart and release chart. "+
				"offered is true for an active release whose release chart is published, on a provider line create_cluster supports; note says why not. "+
				"Without the Release CRD the list is empty and a note names it."),
		mcp.WithString(argProvider, mcp.Description("Only this provider line's releases (aws, eks, azure, …); default every line")),
		readOnly(),
	), t.listReleases)

	s.AddTool(mcp.NewTool(ToolCreateCluster,
		tooldesc.Describe("Create a workload cluster in an organization from a Giant Swarm release, as you or, in mode commit, as a pull request.",
			"Composes the release chart's HelmRelease, OCIRepository and <name>-values ConfigMap in org-<organization>, checked against the release chart's schema before any write. "+
				"The cluster trusts the installation's identity provider, and binds you and the organization's admins as cluster-admin; values with their own global.controlPlane.oidc or bindings win whole. "+
				"Each refusal says why and the way out. A re-run with the same arguments changes nothing; create_node_pool adds GPU pools."),
		mcp.WithString(argOrganization, mcp.Required(), mcp.Description("The organization the cluster belongs to; it lives in the namespace org-<organization>")),
		mcp.WithString(argName, mcp.Required(), mcp.Description(fmt.Sprintf("The cluster's name: a DNS label of at most %d characters starting with a letter, unused on the installation; it cannot change", compose.MaxClusterNameLength))),
		mcp.WithString(argProvider, mcp.Required(), mcp.Enum(compose.Providers()...), mcp.Description("The provider line: its release chart is release-<provider>")),
		mcp.WithString(argRelease, mcp.Description("The release version (36.0.0); default the newest active release of the provider line — list_releases names them")),
		mcp.WithString(argIdentity, mcp.Description("The cloud identity the cluster runs under, never a credential (aws, eks: AWSClusterRoleIdentity; azure: AzureClusterIdentity); default the chart's")),
		mcp.WithString(argDescription, mcp.Description("A sentence on the cluster's purpose (global.metadata.description)")),
		mcp.WithObject(argValues, mcp.Description("The cluster's further values (global.controlPlane, global.nodePools, …), checked with the composed ones against the release chart's schema")),
		modeArg("apply: create the objects as you; commit: open the pull request as you in the repository that owns the organization (get_info modes.commit)"),
		dryRunArg("Return the rendered manifests without writing; in mode commit also the files, the repository, the branch and the directory"),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	), t.createCluster)

	s.AddTool(mcp.NewTool(ToolDeleteCluster,
		tooldesc.Describe("Delete a workload cluster create_cluster created, with its GPU pools and serving, in two calls with the same arguments.",
			"The first call removes its model backend registration, HelmRelease, OCIRepository and values, without waiting for the uninstall; the answer names what goes with it, the models served and nextStep. "+
				"Once list_clusters no longer lists it, the second call removes what the uninstall left, and then answers not found with notFound.nothingLeft. "+
				"Refused with the way out for a cluster cluster-manager did not create, the installation's own, and one Flux applies from git."),
		mcp.WithString(argOrganization, mcp.Required(), mcp.Description("The organization the cluster belongs to; it lives in the namespace org-<organization>")),
		mcp.WithString(argName, mcp.Required(), mcp.Description("The cluster's name as given to create_cluster")),
		modeArg("apply: remove the objects as you; commit: open the removal pull request as you, the backend removed live; commit.liveSteps names what is left"),
		dryRunArg("List what would be removed; nothing is written; in mode commit also the files, the repository, the branch and the directory"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.deleteCluster)

	s.AddTool(mcp.NewTool(ToolListNodePools,
		tooldesc.Describe("List one cluster's node pools with their lifecycle, nodes, Kubernetes versions, instance sizes and prices.",
			"phase is creating, ready, scaling, removing or failed; steps say where the pool release, the MachinePool, the nodes and a prewarm placeholder stand, "+
				"naming what holds a node back (the pods a drain waits on, a launch Karpenter refused and the way around it) and the idle nodes delete_node_pool removes. "+
				"The pool's and the control plane's Kubernetes versions are given side by side, without a verdict. "+
				"Each size carries its on-demand price per hour in the cluster's region, or priceNote."),
		clusterArg(),
		namespaceArg(),
		readOnly(),
	), t.listNodePools)

	s.AddTool(mcp.NewTool(ToolCreateNodePool,
		tooldesc.Describe("Create or update a GPU node pool for a cluster, with the GPU operator and the model serving slice it needs.",
			"A second call with the same name is the update; dryRun shows the difference. "+
				"Composes the gpu-node-pool release from the cluster's current release and, where missing, the <cluster>-gpu-operator release and the cluster's one serving slice (<cluster>-agent-platform), and registers the cluster's kserve backend with model-manager. "+
				"The answer lists each size with what it leaves a predictor, its local disk and on-demand price, and the smallest size that hosts each serving preset (presetFit)."),
		clusterArg(),
		namespaceArg(),
		mcp.WithString(argName, mcp.Required(), mcp.Pattern(compose.PoolNamePattern.String()), mcp.Description("Pool name within the cluster, five to twenty lowercase characters, digits and dashes (gpu00, gpu-l4); <cluster>-<name> names the MachinePool")),
		mcp.WithString(argAccelerator, mcp.Enum(compose.Accelerators...), mcp.DefaultString(compose.Accelerators[0]), mcp.Description("Accelerator from the curated list; picks the EC2 instance family")),
		mcp.WithArray(argSizes, mcp.Items(map[string]any{"type": "string"}), mcp.Description("Instance sizes Karpenter may pick within the family, smallest first; default the chart's: "+strings.Join(compose.DefaultPoolSizes, ", "))),
		mcp.WithNumber(argMaxGPUs, mcp.Min(1), mcp.DefaultNumber(defaultMaxGPUs), mcp.Description("Upper bound of the pool in GPUs across all of its nodes; the minimum is always 0 (scale to zero)")),
		mcp.WithString(argChartVersion, mcp.Description("Exact gpu-node-pool chart version to pin; default none: the pool follows the chart from "+compose.MinPoolChartVersion+" to the next major")),
		mcp.WithBoolean(argPrewarm, mcp.DefaultBool(false), mcp.Description("Launch the pool's first node with the release, held by a placeholder until the first workload; only for the installation's own pool, at install")),
		mcp.WithArray(argZones, mcp.Items(map[string]any{"type": "string"}), mcp.Description("Availability zones the pool's nodes may launch in, of the cluster's node subnets (list_clusters zones); with cache on, the pool follows its cache")),
		mcp.WithBoolean(argCache, mcp.DefaultBool(false), mcp.Description("Whether the cluster's slice, for every pool, mounts a model cache claim: a volume billed monthly until remove_model_cache. Omitted: keep the setting")),
		mcp.WithBoolean(argServing, mcp.DefaultBool(true), mcp.Description("Whether the pool serves models: false creates a plain GPU pool, with no serving slice and no backend registration")),
		modeArg("apply: land the objects as you; commit: open the pull request as you (list_clusters commitTarget), the backend registered live"),
		dryRunArg("Render and compare only; nothing is written"),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	), t.createNodePool)

	s.AddTool(mcp.NewTool(ToolDeleteNodePool,
		tooldesc.Describe("Delete a GPU node pool create_node_pool created, idle nodes first; with the cluster's last pool, its model serving too.",
			"Refused while a node of the pool is busy unless force, naming each busy node and the models to unload first with model-manager's unload_model, and while a served model waits for a node of this pool. "+
				"Does not wait for the instances to terminate: list_node_pools shows them. With a pool that is not the last, the backend registration is re-written for the pools left. "+
				"Answers within the caller's deadline: partial and nextStep name the re-run that continues."),
		clusterArg(),
		namespaceArg(),
		mcp.WithString(argName, mcp.Required(), mcp.Description("Pool name as given to create_node_pool")),
		mcp.WithBoolean(argForce, mcp.DefaultBool(false), mcp.Description("Delete even while the pool runs nodes: their workloads, served models included, are evicted")),
		modeArg("apply: remove the objects as you, refused for a pool Flux applies from git; commit: open the removal pull request, the backend changed live"),
		dryRunArg("List what would be removed; nothing is written"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.deleteNodePool)

	s.AddTool(mcp.NewTool(ToolEnableModelServing,
		tooldesc.Describe("Switch model serving on for a cluster, with or without a GPU pool: its one serving slice, <cluster>-agent-platform.",
			fmt.Sprintf("The slice (KServe, the llm-d control plane, the models Gateway at models.<domain>) follows the agent-platform chart from the version the installation's platform release runs, at least %s, to the next major; ", compose.MinSliceChartVersion)+
				"the cluster's kserve backend is registered with model-manager. An existing release is updated in place. "+
				"Refused where the platform's own release or a hand install provides serving already. dryRun returns the rendered manifests and writes nothing."),
		clusterArg(),
		namespaceArg(),
		mcp.WithBoolean(argCache, mcp.DefaultBool(false), mcp.Description("Whether the slice mounts a model cache claim: a volume billed monthly until remove_model_cache. Omitted: keep the release's setting")),
		modeArg(applyOnly("land the objects on the installation as you")),
		dryRunArg("Render and compare only; nothing is written"),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.enableModelServing)

	s.AddTool(mcp.NewTool(ToolDisableModelServing,
		tooldesc.Describe("Switch model serving off for a cluster: remove the serving slice and model backend cluster-manager created.",
			"Refused while models are served on the cluster unless force (the message names them), when the cluster cannot be read as you, and for a slice cluster-manager did not create. "+
				"The slice goes in order; last the serving namespace is removed, or marked retired, naming what holds it. The model cache claims stay: remove_model_cache removes them. "+
				"Answers within the caller's deadline: partial and nextStep name the re-run that continues. dryRun lists what would be removed."),
		clusterArg(),
		namespaceArg(),
		mcp.WithBoolean(argForce, mcp.DefaultBool(false), mcp.Description("Remove the serving slice even while models are served: they go with it")),
		modeArg(applyOnly("remove the objects from the installation as you")),
		dryRunArg("List what would be removed; nothing is written"),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.disableModelServing)

	s.AddTool(mcp.NewTool(ToolRemoveModelCache,
		tooldesc.Describe("Remove a cluster's model cache: its hf-cache* volume claims, billed every month they exist, even with no pool left.",
			"Where cluster-manager's serving slice mounts the cache, it is switched to serve without it first; every predictor then downloads its weights at each start. "+
				"Refused while a pod mounts a claim to be removed (the refusal names the models to unload with model-manager's unload_model), when the cluster cannot be read as you, and where someone else provides serving. "+
				"The answer lists the claims removed with their size and monthly price. dryRun lists what would be removed."),
		clusterArg(),
		namespaceArg(),
		mcp.WithString(argClaim, mcp.Description("One model cache claim to remove, by name (hf-cache, hf-cache-eu-central-1a); default every hf-cache* claim of the serving namespace")),
		modeArg(applyOnly("remove the objects from the installation and the cluster as you")),
		dryRunArg("List what would be removed; nothing is written"),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), t.removeModelCache)

	return s
}

// readOnly annotates a tool that changes nothing; mcp-go defaults every tool
// to destructive and not idempotent.
func readOnly() mcp.ToolOption {
	return func(t *mcp.Tool) {
		mcp.WithReadOnlyHintAnnotation(true)(t)
		mcp.WithDestructiveHintAnnotation(false)(t)
		mcp.WithIdempotentHintAnnotation(true)(t)
	}
}

// clusterArg and namespaceArg name the cluster a tool acts on.
func clusterArg() mcp.ToolOption {
	return mcp.WithString(argCluster, mcp.Required(), mcp.Description("Cluster name"))
}

func namespaceArg() mcp.ToolOption {
	return mcp.WithString(argNamespace, mcp.Description("Cluster namespace (org-<organization>); optional when the name is unique on the installation"))
}

// modeArg is a write tool's mode, apply by default.
func modeArg(desc string) mcp.ToolOption {
	return mcp.WithString(argMode, mcp.Enum(tools.ModeApply, tools.ModeCommit), mcp.DefaultString(tools.ModeApply), mcp.Description(desc))
}

// applyOnly describes the mode of a tool that refuses commit.
func applyOnly(apply string) string {
	return "apply: " + apply + "; commit is refused here (get_info modes.commitTools)"
}

func dryRunArg(desc string) mcp.ToolOption {
	return mcp.WithBoolean(argDryRun, mcp.DefaultBool(false), mcp.Description(desc))
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
			ChartVersion: req.GetString(argChartVersion, ""),
			Prewarm:      req.GetBool(argPrewarm, false),
			Zones:        req.GetStringSlice(argZones, nil),
		},
		Cache:     optionalBool(req, argCache),
		NoServing: !req.GetBool(argServing, true),
		Mode:      req.GetString(argMode, tools.ModeApply),
		DryRun:    req.GetBool(argDryRun, false),
	}
	result, err := h.svc.CreateNodePool(ctx, in)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(result)
}

func (h *handlers) listReleases(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	releases, err := h.svc.ListReleases(ctx, req.GetString(argProvider, ""))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(releases)
}

func (h *handlers) createCluster(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	in := tools.CreateClusterInput{
		Release:     req.GetString(argRelease, ""),
		Identity:    req.GetString(argIdentity, ""),
		Description: req.GetString(argDescription, ""),
		Mode:        req.GetString(argMode, tools.ModeApply),
		DryRun:      req.GetBool(argDryRun, false),
	}
	for _, a := range []struct {
		name string
		dst  *string
	}{{argOrganization, &in.Organization}, {argName, &in.Name}, {argProvider, &in.Provider}} {
		v, err := req.RequireString(a.name)
		if err != nil {
			return errResult(err), nil
		}
		*a.dst = v
	}
	if raw, given := req.GetArguments()[argValues]; given && raw != nil {
		values, ok := raw.(map[string]any)
		if !ok {
			return errResult(fmt.Errorf("values: want an object, got %T", raw)), nil
		}
		in.Values = values
	}
	result, err := h.svc.CreateCluster(ctx, in)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(result)
}

func (h *handlers) deleteCluster(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	in := tools.DeleteClusterInput{
		Mode:   req.GetString(argMode, tools.ModeApply),
		DryRun: req.GetBool(argDryRun, false),
	}
	var err error
	if in.Organization, err = req.RequireString(argOrganization); err != nil {
		return errResult(err), nil
	}
	if in.Name, err = req.RequireString(argName); err != nil {
		return errResult(err), nil
	}
	result, err := h.svc.DeleteCluster(ctx, in)
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
	return jsonResult(Info{Version: h.version, Modes: Modes{Apply: true, Commit: h.svc.CommitAvailable(), CommitTools: commitTools}, Tools: ToolNames(), ClusterAPI: h.svc.ClusterAPI(ctx)})
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
		text := err.Error() + ": list_clusters names the clusters you may see"
		if notFound.NotFound != nil {
			// The structured not-found beside the text: a second text
			// content carrying {"notFound": {cluster, namespace, nothingLeft}}.
			block, _ := json.Marshal(map[string]any{"notFound": notFound.NotFound})
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.NewTextContent(text), mcp.NewTextContent(string(block))}}
		}
		return mcp.NewToolResultError(text)
	}
	return mcp.NewToolResultError(err.Error())
}
