package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// Resources the write tools read besides the read tools'.
var (
	ConfigMapGVR = compose.ConfigMapGVR
	AppGVR       = detect.AppGVR
)

// Write modes. Only apply exists in this stage; commit (a pull request as
// the person) arrives with the epic's next proof (bumblebee-plans#46 D11).
const (
	ModeApply  = "apply"
	ModeCommit = "commit"
)

// Release CR components the pool's pins come from, and the machine image
// they form: `flatcar-<channel>-<flatcar>-kube-<k8s>-tooling-<tooling>-gs`,
// the name cluster-aws builds for its own workers (the controller looks the
// AMI up by name, per region).
const (
	releaseComponentFlatcar   = "flatcar"
	releaseComponentOSTooling = "os-tooling"
	osReleaseChannel          = "stable"

	// labelKustomizeName marks an object Flux's kustomize-controller owns.
	labelKustomizeName = "kustomize.toolkit.fluxcd.io/name"
)

// ErrRefused is a refusal with the fix in the message: a mode not offered, a
// version skew, a GitOps-owned object, nodes still busy, zones against the
// model cache.
type ErrRefused struct {
	Reason string
	// Refused is the structured form of the nodes guard's and the zones
	// refusal, beside the text; nil for the other refusals.
	Refused *Refused
}

// Refused is a refusal as the portal renders it without parsing prose:
// delete_node_pool's nodes guard — the pool's busy nodes, its idle ones, the
// models served on the cluster, the hint, and what the guard read — and
// create_node_pool's zones against the model cache (CacheZone, CacheClaims).
type Refused struct {
	// Nodes are the pool's busy nodes by name — by provider id when read
	// from the MachinePool.
	Nodes []string `json:"nodes"`
	// Idle are the pool's idle nodes: they go with the pool once the busy
	// ones are free.
	Idle   []string `json:"idle,omitempty"`
	Models []string `json:"models"`
	// Unscheduled are the served models of model-manager's whose predictor
	// runs on no node — Pending, waiting for a node of the pool — when
	// they, and no busy node, refused the delete (giantswarm/cluster-manager#59).
	Unscheduled []string `json:"unscheduled,omitempty"`
	Hint        string   `json:"hint"`
	// ReadFrom is what the guard judged from: `cluster` (the pool's
	// NodeClaims and Nodes with the pods on them, read as the caller) or
	// `machinePool` (the MachinePool's provider IDs, when the cluster cannot
	// be read as the caller).
	ReadFrom string `json:"readFrom"`
	// CacheZone is create_node_pool's refusal of the zones named against the
	// model cache: several zones with the cache on, or the zone's claim Bound
	// elsewhere (giantswarm/cluster-manager#65, #71); nil for the others.
	CacheZone *CacheZoneRefusal `json:"cacheZone,omitempty"`
	// CacheClaims is create_node_pool's refusal of no zones while the
	// serving namespace has several model cache claims
	// (giantswarm/cluster-manager#71); nil for the others.
	CacheClaims *CacheClaimsRefusal `json:"cacheClaims,omitempty"`
	// CacheOn is the refusal of `cache: false` while the cluster's slice
	// release runs with the cache on — the setting is the cluster's, and the
	// way to serve without the cache is to remove it
	// (giantswarm/cluster-manager#83); nil for the others.
	CacheOn *CacheOnRefusal `json:"cacheOn,omitempty"`
}

// What the nodes guard read from (Refused.ReadFrom).
const (
	readFromCluster     = "cluster"
	readFromMachinePool = "machinePool"
)

// The hints of a refusal: what to expect after unloading.
const (
	refusedHint            = "Unload the served model(s) and re-run: delete_node_pool removes the pool's idle nodes itself and completes the teardown in one call."
	refusedHintMachinePool = "Karpenter removes an empty node about 10 minutes after its last pod and the MachinePool's list follows minutes later; a served model has to be unloaded first. With the cluster readable as you, delete_node_pool removes idle nodes itself."
)

func (e *ErrRefused) Error() string { return e.Reason }

// CreateNodePoolInput is create_node_pool's input.
type CreateNodePoolInput struct {
	Cluster   string
	Namespace string
	Pool      compose.PoolSpec
	// Cache is whether the slice's predictors mount a model cache claim
	// (false composes compose.SliceSpec.NoCache); nil keeps the slice's
	// setting where the cluster's slice release runs, off for a first slice
	// (cacheChoice). Pool.Zones are the zones the caller named: one zone,
	// whose claim the slice mounts, judged against the claims as read
	// (zonePinFor).
	Cache  *bool
	Mode   string
	DryRun bool
}

// cacheChoice is the model cache setting a write composes: the caller's where
// given; else the slice's own where the cluster's slice release runs — the
// setting is the cluster's (giantswarm/cluster-manager#83) — and off for a
// first slice: a claim is a volume billed every month it exists, after every
// pool is removed too, and is never created unasked (giantswarm/cluster-manager#86).
func cacheChoice(given *bool, before *detect.SliceCache) bool {
	if given != nil {
		return *given
	}
	return before != nil && before.Enabled
}

// DeleteNodePoolInput is delete_node_pool's input.
type DeleteNodePoolInput struct {
	Cluster   string
	Namespace string
	Name      string
	Force     bool
	Mode      string
	DryRun    bool
}

// WriteResult is the answer of a write: what was (or, dry-run, would be)
// done per object, and the rendered manifests.
type WriteResult struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	// Pool is the pool a node-pool write concerns; empty for the model
	// serving writes.
	Pool   string `json:"pool,omitempty"`
	Mode   string `json:"mode"`
	DryRun bool   `json:"dryRun"`
	// Pins of a created pool; empty on delete.
	ChartVersion        string         `json:"chartVersion,omitempty"`
	KubernetesVersion   string         `json:"kubernetesVersion,omitempty"`
	ControlPlaneVersion string         `json:"controlPlaneVersion,omitempty"`
	MachineImage        string         `json:"machineImage,omitempty"`
	Objects             []ObjectAction `json:"objects"`
	// Manifests are the rendered objects (create) — the dry-run's answer.
	Manifests []map[string]any `json:"manifests,omitempty"`
	// GPUOperator is the operator detected on the cluster before the write
	// (create): present with its provider — nothing is composed —, or
	// absent, in which case OperatorRow names the configuration table's row
	// the `<cluster>-gpu-operator` release was composed from.
	GPUOperator detect.Component `json:"gpuOperator,omitempty"`
	OperatorRow string           `json:"operatorRow,omitempty"`
	// Serving is the serving layer detected on the cluster before the write
	// (create, enable): present with its provider — nothing is composed —,
	// or absent or cluster-manager's own, in which case Slice describes the
	// `<cluster>-agent-platform` release composed (created or updated in
	// place).
	Serving detect.Component `json:"serving,omitempty"`
	Slice   *SliceRelease    `json:"slice,omitempty"`
	// Backend is the kserve backend registered with model-manager (create).
	Backend *BackendRegistration `json:"backend,omitempty"`
	// LastPool marks a delete of the cluster's last GPU pool: the operator
	// and slice releases cluster-manager created and the backend it
	// registered go too — the slice release only when no other slice is on
	// in it (SliceKept names it then).
	LastPool  bool   `json:"lastPool,omitempty"`
	SliceKept string `json:"sliceKept,omitempty"`
	// Sizes are the pool's instance sizes as composed (create): the node as
	// AWS lists it, its NVMe instance store (what a pool node's /var/lib is
	// from gpu-node-pool 0.7.0), what it leaves a predictor after the
	// kubelet's reservations and the fleet's daemonsets, and its on-demand
	// price per hour in the cluster's region (giantswarm/cluster-manager#44). PresetFit
	// places the serving presets against them — the ones published on the
	// cluster, else the ones the slice would publish, read from its chart;
	// Warnings name the presets the accelerator could serve but no size of
	// the pool hosts (giantswarm/agent-platform#502).
	Sizes     []compose.InstanceShape `json:"sizes,omitempty"`
	PresetFit *PresetFit              `json:"presetFit,omitempty"`
	Warnings  []string                `json:"warnings,omitempty"`
	// Zones are the availability zones the pool's nodes are pinned to
	// (create): the zone the caller named, or the zone the serving
	// namespace's one model cache claim is bound to — a claim is one volume
	// in one zone, which a node elsewhere strands a predictor mounting
	// (giantswarm/cluster-manager#59, #65, #71). Empty for no pin; ZonesNote
	// says in a sentence whose the pin is, what was found and what it means.
	// CacheClaim is the claim the slice mounts as read (null while it does
	// not exist yet, or with the cache off), CacheClaims every model cache
	// claim of the serving namespace as read (null when they cannot be
	// read). Cache is the model cache setting of the slice the write composed
	// (create, enable): whether the predictors mount a claim, which, and what
	// follows; null when no slice was composed.
	Zones       []string             `json:"zones,omitempty"`
	ZonesNote   string               `json:"zonesNote,omitempty"`
	CacheClaim  *detect.CacheClaim   `json:"cacheClaim,omitempty"`
	CacheClaims []*detect.CacheClaim `json:"cacheClaims,omitempty"`
	Cache       *CacheSetting        `json:"cache,omitempty"`
	// RemovedClaims are the model cache claims remove_model_cache deleted
	// (or, dry-run, would delete), as read before the delete with their
	// price: what stops being billed (giantswarm/cluster-manager#83).
	RemovedClaims []*detect.CacheClaim `json:"removedClaims,omitempty"`
	// Partial marks an apply that stopped writing so its answer arrives
	// within the caller's deadline: the objects it did not reach are listed
	// with action `pending`, and NextStep says what to do — re-run, the
	// pending objects are written first (giantswarm/cluster-manager#34).
	Partial  bool   `json:"partial,omitempty"`
	NextStep string `json:"nextStep,omitempty"`
}

// BackendRegistration is the backend document create_node_pool writes.
type BackendRegistration struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Target is the cluster the backend reaches: `local` for the
	// installation's own cluster, else the cluster and its apiserver.
	Target string `json:"target"`
}

// ObjectAction is what happened to one object.
type ObjectAction struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	// Action is create, update, unchanged or delete (would-… when dry-run);
	// pending when a partial apply did not reach the object.
	Action string `json:"action"`
	// Changes are the spec paths an update changes — the dry-run's drift
	// check on a re-run.
	Changes []string `json:"changes,omitempty"`
}

// String names the object: `namespace/name`, the name alone for a
// cluster-scoped one.
func (a ObjectAction) String() string {
	if a.Namespace == "" {
		return a.Name
	}
	return a.Namespace + "/" + a.Name
}

// CreateNodePool composes the pool release for the cluster's current Release
// CR and values and lands it as the caller (apply mode). A second call on the
// same name is the update; its dry-run shows the difference. When no GPU
// operator runs on the cluster it composes the `<cluster>-gpu-operator`
// release beside the pool, configured from the two-row table; a chart-provided
// operator is never re-created. Where nothing provides serving it composes
// the cluster's `<cluster>-agent-platform` slice release with the serving
// slice on (updated in place on a re-run); a chart-provided serving layer is
// left alone. After the pool it registers the cluster's kserve backend with
// model-manager — the document carrying the pool's instance shapes when it
// is the cluster's only pool, for the fit check at scale-from-zero
// (giantswarm/cluster-manager#26). The answer lists the pool's sizes with what each leaves a
// predictor and, where the cluster publishes serving presets, the smallest
// size that hosts each — a preset no size hosts is a warning
// (giantswarm/agent-platform#502). The pool's nodes are pinned to the zone
// the caller named — checked against the cluster's node subnets — and the
// slice mounts that zone's model cache claim, or, with none named, to the
// zone of the one claim bound in the serving namespace
// (compose.PoolSpec.Zones; zonePinFor), and the answer says so; with cache
// false the slice serves without a claim and no zone follows from one
// (giantswarm/cluster-manager#59, #65, #71).
// LLMInferenceServiceConfigs a serving layer that went left terminating in
// the release namespace are healed before the slice lands
// (giantswarm/cluster-manager#28). Every refusal comes before any write.
//
// The call answers within the aggregator's deadline for a tool call
// (giantswarm/cluster-manager#34): everything it reads is read once,
// concurrently, before anything is composed; the objects land in the order
// pool, slice, the slice's backend registration, operator — the ConfigMap
// right after the slice's HelmRelease, so a call cut short never leaves a
// slice model-manager does not know —, the objects that do not exist yet
// before the updates; and a write there is no budget left for is left
// pending rather than started (WriteResult.Partial). Every phase is timed in
// the log at debug.
func (s *Service) CreateNodePool(ctx context.Context, in CreateNodePoolInput) (*WriteResult, error) {
	start := time.Now()
	if err := checkMode(in.Mode); err != nil {
		return nil, err
	}
	k := s.clients(ctx)
	c, err := s.getCluster(ctx, k, in.Cluster, in.Namespace)
	if err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	shapes, err := compose.Shapes(in.Pool.Accelerator, in.Pool.Sizes)
	if err != nil {
		return nil, err
	}
	target := s.target(ctx, dyn, c)
	r, err := s.readPool(ctx, dyn, target, c, in, shapes)
	if err != nil {
		return nil, err
	}
	facts := r.facts
	if newer, err := versionNewer(facts.KubernetesVersion, strings.TrimPrefix(r.cpVersion, "v")); err == nil && newer {
		return nil, &ErrRefused{Reason: fmt.Sprintf("the cluster's release pins Kubernetes %s but its control plane runs %s: a pool is never newer than the control plane — finish the cluster's upgrade first, then re-run", facts.KubernetesVersion, r.cpVersion)}
	}
	cache := cacheChoice(in.Cache, r.slice.cache)
	if err := r.aws.checkZones(in.Pool.Zones, target.Cluster); err != nil {
		return nil, err
	}
	// The cache is the cluster's setting: off while the cluster's slice runs
	// with it on is refused, the way out being to remove the cache
	// (giantswarm/cluster-manager#83).
	if !cache && r.slice.cache != nil && r.slice.cache.Enabled {
		return nil, cacheOnRefusal(r.cache, target.Cluster, r.slice.release, r.slice.cache, r.aws.region)
	}
	pin, err := zonePinFor(r.cache, target.Cluster, zoneChoice{zones: in.Pool.Zones, cache: cache})
	if err != nil {
		return nil, err
	}
	in.Pool.Zones = pin.zones
	objs, err := compose.Pool(facts, in.Pool)
	if err != nil {
		return nil, &ErrRefused{Reason: err.Error()}
	}
	operator, row, operatorObjs, err := s.operatorRelease(r.operator, target, facts, r.pools)
	if err != nil {
		return nil, err
	}
	pinned := onlyOf(r.pools)
	serving, slice, sliceObjs, err := s.sliceRelease(r.slice, target, facts, pinned, pin.sliceCache())
	if err != nil {
		return nil, err
	}
	if slice == nil && !cache {
		return nil, &ErrRefused{Reason: fmt.Sprintf("cache false: serving on %s is provided by %s (%s), not composed by cluster-manager — the model cache is that serving layer's setting, not this pool's; leave cache out, or change the setting where that layer is configured", target.Cluster, providerDescription(serving.Provider), strings.Join(serving.Evidence, "; "))}
	}
	// The backend document names the sizes of the pool the predictors are
	// pinned to: this one when it is the cluster's only pool; with several
	// none is pinned and the document names no sizes, so a re-run never
	// judges a load against a pool it may not land on.
	var instances []compose.InstanceShape
	if pinned != "" {
		instances = shapes
	}
	backend, err := s.backendDocument(target, instances, r.backend)
	if err != nil {
		return nil, err
	}
	// The claims priced in the cluster's region, and the cache block's
	// figures for the claim the slice mounts — as read, or as the chart
	// would create it (giantswarm/cluster-manager#83).
	claims := r.cache.priced(r.aws.region, r.slice.cache)
	cacheWords := s.cacheFacts(ctx, target.Cluster, r.aws.region, r.slice, slice, pin)
	out := &WriteResult{
		Cluster: c.GetName(), Namespace: c.GetNamespace(), Pool: in.Pool.Name, Mode: in.Mode, DryRun: in.DryRun,
		ChartVersion: nestedString(objs[0], "spec", "ref", "tag"), KubernetesVersion: facts.KubernetesVersion,
		ControlPlaneVersion: r.cpVersion, MachineImage: facts.MachineImage, Objects: []ObjectAction{},
		GPUOperator: operator, OperatorRow: row, Serving: serving, Slice: slice,
		Backend: &BackendRegistration{Kind: compose.BackendKindKServe, Namespace: backend.GetNamespace(), Name: backend.GetName(), Target: backendTargetName(target.backend)},
		Sizes:   compose.Priced(shapes, r.aws.region), PresetFit: r.fit, Warnings: pin.warnings(r.warnings),
		Zones: pin.zones, ZonesNote: pin.note, CacheClaim: pin.claim, CacheClaims: claims, Cache: s.cacheSettingFor(slice, r.cache, pin, cacheWords),
	}
	// Configs a serving layer that went left terminating in the release
	// namespace break the slice about to be composed: healed first, before
	// anything lands.
	if slice != nil {
		if err := healStrandedConfigs(ctx, target, in.DryRun, out); err != nil {
			return nil, err
		}
	}
	// A pool that used to carry credentials and no longer does: the stale
	// Secret goes.
	if len(facts.RegistryCredentials) == 0 {
		if act, err := deleteIfOwned(ctx, dyn, compose.SecretGVR, c.GetNamespace(), compose.ValuesSecretName(c.GetName(), in.Pool.Name), in.DryRun); err != nil {
			return nil, err
		} else if act != nil {
			out.Objects = append(out.Objects, *act)
		}
	}
	objs = append(objs, sliceObjs...)
	objs = append(objs, backend)
	objs = append(objs, operatorObjs...)
	if err := applyAll(ctx, dyn, objs, in.DryRun, out, s.budget(ctx, start)); err != nil {
		return nil, err
	}
	logApplied(ctx, "create_node_pool", out, start)
	return out, nil
}

// poolReads is everything create_node_pool reads before it composes: each
// independent of the others, so they are read concurrently, once per call.
type poolReads struct {
	facts     compose.Cluster
	cpVersion string
	pools     []string
	operator  operatorReads
	slice     sliceReads
	// backend is model-manager's kserve backend document as it exists on
	// the installation, nil for none.
	backend  *unstructured.Unstructured
	fit      *PresetFit
	warnings []string
	// aws is the cluster's AWSCluster: the region for the sizes' prices, the
	// node subnets' zones the caller's zones are checked against.
	aws awsInfra
	// cache is the serving namespace's model cache claims on the target: the
	// pool's zone pin and the claim its slice mounts.
	cache cacheClaims
}

// readPool reads the pool's inputs concurrently: the cluster's facts, its
// control plane version, its pool releases, what the target runs (operator,
// serving, presets), the platform's inputs and the backend registered.
func (s *Service) readPool(ctx context.Context, dyn dynamic.Interface, t target, c *unstructured.Unstructured, in CreateNodePoolInput, shapes []compose.InstanceShape) (*poolReads, error) {
	defer timed(ctx, "reads")()
	r := &poolReads{}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		defer timed(gctx, "cluster facts")()
		r.facts, err = s.clusterFacts(gctx, dyn, c)
		return err
	})
	g.Go(func() error {
		defer timed(gctx, "control plane version")()
		r.cpVersion = controlPlaneVersion(gctx, dyn, c)
		return nil
	})
	g.Go(func() (err error) {
		defer timed(gctx, "pool releases")()
		r.pools, err = poolNames(gctx, dyn, c.GetNamespace(), c.GetName(), in.Pool.Name)
		return err
	})
	g.Go(func() error {
		defer timed(gctx, "operator detection")()
		r.operator = readOperator(gctx, t)
		return nil
	})
	g.Go(func() (err error) {
		defer timed(gctx, "serving detection and platform inputs")()
		r.slice, err = s.readSlice(gctx, dyn, t)
		return err
	})
	g.Go(func() (err error) {
		defer timed(gctx, "registered backend")()
		r.backend, err = existingBackend(gctx, dyn, s.cfg.ModelManagerNamespace)
		return err
	})
	g.Go(func() error {
		defer timed(gctx, "preset fit")()
		r.fit, r.warnings = s.presetFit(gctx, t, in.Pool.Name, shapes)
		return nil
	})
	g.Go(func() error {
		defer timed(gctx, "aws infrastructure")()
		r.aws = awsInfrastructure(gctx, dyn, c)
		return nil
	})
	g.Go(func() error {
		defer timed(gctx, "cache claims")()
		r.cache = s.readCacheClaims(gctx, t)
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	// Nothing published on the target and a slice about to be composed: the
	// presets that slice would publish, from the chart it pins — the version
	// is the platform's, known only now (giantswarm/cluster-manager#44).
	if r.fit != nil && r.fit.unpublished && composesSlice(r.slice.serving) {
		defer timed(ctx, "preset fit from the chart")()
		version, err := compose.SliceChartVersion(compose.SliceSpec{ChartVersion: s.cfg.SliceChartVersion, Platform: r.slice.platform})
		if err == nil {
			r.fit, r.warnings = s.presetFitFromChart(ctx, r.fit, version, in.Pool.Name, shapes)
		}
	}
	return r, nil
}

// operatorReads is what the operator's part of a pool reads: the operator
// detected on the target and, where the release would be composed, the
// target's nodes the configuration table's row is derived from.
type operatorReads struct {
	component detect.Component
	nodes     []detect.Node
	nodesErr  error
}

func readOperator(ctx context.Context, t target) operatorReads {
	r := operatorReads{component: detect.GPUOperator(ctx, t.Target)}
	if composesOperator(r.component, t) {
		r.nodes, r.nodesErr = detect.Nodes(ctx, t.Reader)
	}
	return r
}

// composesOperator reports whether the operator's part of a pool is composed:
// when no operator runs on the target, or the one running is cluster-manager's
// own (the re-run is its update). Not when someone else provides it (the
// platform's chart, a human), nor when the target cannot be read.
func composesOperator(operator detect.Component, t target) bool {
	if operator.Status == detect.StatusUnknown {
		return false
	}
	return !operator.Present() || (operator.Provider == detect.ProviderClusterManager && t.Reader != nil)
}

// operatorRelease decides the operator's part of a pool from what was read:
// nothing when one runs on the target and someone else provides it, the
// `<cluster>-gpu-operator` release from the table's row when none does — or
// when the one running is cluster-manager's own, so the re-run is its
// update, which also moves the worker's pin to the cluster's pools (pools) —,
// a refusal when the cluster cannot be read or its nodes match no row.
func (s *Service) operatorRelease(r operatorReads, t target, facts compose.Cluster, pools []string) (detect.Component, string, []*unstructured.Unstructured, error) {
	operator := r.component
	switch {
	case operator.Status == detect.StatusUnknown:
		return operator, "", nil, &ErrRefused{Reason: fmt.Sprintf("cannot tell whether a GPU operator runs on %s (%s): the operator is composed only when none does — make the cluster readable as you (its apiserver must trust the installation's identity provider) and re-run", t.Cluster, operator.Reason)}
	case !composesOperator(operator, t):
		return operator, "", nil, nil
	case r.nodesErr != nil:
		return operator, "", nil, &ErrRefused{Reason: fmt.Sprintf("no GPU operator runs on %s and its nodes are not readable as you (%v): the operator's configuration is read from them — re-run once you may list the cluster's nodes", t.Cluster, r.nodesErr)}
	}
	row, err := compose.DeriveOperatorRow(detect.ComposeNodes(r.nodes), facts.MachineImage)
	if err != nil {
		return operator, "", nil, &ErrRefused{Reason: err.Error()}
	}
	return operator, row.Name, compose.Operator(facts, row, pools, compose.OperatorOptions{DCGMExporter: s.cfg.OperatorDCGMExporter}), nil
}

// existingBackend is model-manager's kserve backend document as it exists on
// the installation; nil when none is registered.
func existingBackend(ctx context.Context, dyn dynamic.Interface, ns string) (*unstructured.Unstructured, error) {
	cm, err := dyn.Resource(compose.ConfigMapGVR).Namespace(ns).Get(ctx, compose.BackendConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ConfigMap %s/%s: %w", ns, compose.BackendConfigMapName, err)
	}
	return cm, nil
}

// backendDocument renders the kserve backend document for the target — with
// the shapes of the pinned pool, none for no pin — and refuses when
// model-manager's one kserve document (existing, nil for none) is registered
// for another cluster.
func (s *Service) backendDocument(t target, instances []compose.InstanceShape, existing *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if t.backendErr != nil {
		return nil, &ErrRefused{Reason: fmt.Sprintf("the kserve backend of %s cannot be registered with model-manager: %v", t.Cluster, t.backendErr)}
	}
	backend, err := compose.KServeBackend(s.cfg.ModelManagerNamespace, t.backend, instances)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return backend, nil
	}
	if other := existing.GetLabels()[compose.LabelCluster]; compose.OwnedBy(existing) && other != t.Cluster {
		return nil, &ErrRefused{Reason: fmt.Sprintf("model-manager's kserve backend (ConfigMap %s/%s) is registered for cluster %s: model-manager takes one kserve backend per installation — delete that cluster's last GPU pool first, which removes the registration, then re-run", backend.GetNamespace(), backend.GetName(), other)}
	}
	return backend, nil
}

// budget is when an apply that started at start must have answered: the
// request's own deadline when the caller set one, else the configured apply
// budget from the start.
func (s *Service) budget(ctx context.Context, start time.Time) time.Time {
	b := start.Add(s.cfg.applyBudget())
	if d, ok := ctx.Deadline(); ok && d.Before(b) {
		b = d
	}
	return b
}

// timed logs how long one phase of a call took, at debug, when the returned
// function is called — so the next regression in the reads or a slow write is
// visible in the log (giantswarm/cluster-manager#34).
func timed(ctx context.Context, phase string, attrs ...any) func() {
	start := time.Now()
	return func() {
		slog.DebugContext(ctx, "phase done", append([]any{"phase", phase, "duration", time.Since(start).Round(time.Millisecond)}, attrs...)...)
	}
}

// logApplied is the one line per write call: what landed, in how long.
func logApplied(ctx context.Context, tool string, out *WriteResult, start time.Time) {
	written := 0
	for _, o := range out.Objects {
		if o.Action == actionCreate || o.Action == actionUpdate || o.Action == actionDelete {
			written++
		}
	}
	slog.InfoContext(ctx, tool+" done", "cluster", out.Cluster, "pool", out.Pool, "dryRun", out.DryRun, "objects", len(out.Objects), "written", written, "partial", out.Partial, "duration", time.Since(start).Round(time.Millisecond))
}

// backendTargetName is the target as the backend document names it.
func backendTargetName(t compose.BackendTarget) string {
	if t.OwnCluster {
		return compose.BackendTargetLocal
	}
	return t.Cluster + " (" + t.APIServer + ")"
}

// DeleteNodePool removes what create_node_pool created. It refuses while a
// node of the pool is busy unless forced — the refusal names the nodes, what
// holds them and the models served on the cluster (nodesGuard) — and removes
// the pool's idle nodes itself, through their NodeClaims on the cluster,
// before anything else (giantswarm/cluster-manager#49).
// With the cluster's last pool go the operator release cluster-manager
// created, its slice release unless another slice is on in it, and the
// kserve backend it registered. The slice goes in order (servingTeardown):
// the llm-d controller's child release, then its well-known
// LLMInferenceServiceConfigs — removed by cluster-manager once no controller
// runs to deny their delete —, then the configs' child release, the
// operator, the backend registration, the slice release, and the pool's own
// objects last, its HelmRelease the very last: the re-run finds the pool and
// continues where the teardown stands (giantswarm/cluster-manager#28, #37).
//
// The call answers within the aggregator's deadline for a tool call: every
// object is planned before the first write (every refusal first), the writes
// go one at a time in that order, and a write there is no budget left for is
// left pending rather than started (WriteResult.Partial) —
// giantswarm/cluster-manager#34's pattern.
func (s *Service) DeleteNodePool(ctx context.Context, in DeleteNodePoolInput) (*WriteResult, error) {
	start := time.Now()
	if err := checkMode(in.Mode); err != nil {
		return nil, err
	}
	k := s.clients(ctx)
	c, err := s.getCluster(ctx, k, in.Cluster, in.Namespace)
	if err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	ns, release := c.GetNamespace(), compose.ReleaseName(c.GetName(), in.Name)
	hr, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, release, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &ErrNotFound{What: fmt.Sprintf("node pool %s of cluster %s (HelmRelease %s/%s)", in.Name, c.GetName(), ns, release)}
	}
	if err != nil {
		return nil, fmt.Errorf("get HelmRelease %s/%s: %w", ns, release, err)
	}
	if !compose.OwnedBy(hr) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("HelmRelease %s/%s was not created by cluster-manager (%s): delete_node_pool removes only what create_node_pool created — %s", ns, release, ownerDescription(hr), removalHint(hr))}
	}
	t := s.target(ctx, dyn, c)
	var idle []*poolNode
	if !in.Force {
		if idle, err = s.nodesGuard(ctx, dyn, c, release, t); err != nil {
			return nil, err
		}
	}
	targets, slice, kept, last, err := s.removalTargets(ctx, dyn, c, in.Name)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{Cluster: c.GetName(), Namespace: ns, Pool: in.Name, Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{}, LastPool: last, SliceKept: kept}
	plans, err := planDeletes(ctx, dyn, targets)
	if err != nil {
		return nil, err
	}
	td := newTeardown(ctx, dyn, in.DryRun, out, s.budget(ctx, start))
	if err := td.deleteNodeClaims(t.Reader, idle); err != nil {
		return nil, err
	}
	if slice != nil {
		if err := s.servingTeardown(td, t, in.Force); err != nil {
			return nil, err
		}
	}
	if err := td.deleteAll(plans); err != nil {
		return nil, err
	}
	td.finish()
	logApplied(ctx, "delete_node_pool", out, start)
	return out, nil
}

// removalTargets names what delete_node_pool removes for a pool, in teardown
// order: with the cluster's last pool the operator release and its source,
// then the slice's objects — unless another slice shares the release, which
// then stays (kept names it) — then the pool's own source, values Secret and
// release, the release last so the re-run finds it. slice is the slice
// release to tear down in order, nil when none goes. list_node_pools reads
// the same targets to show a removal's pending objects.
func (s *Service) removalTargets(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, pool string) (targets []objectRef, slice *unstructured.Unstructured, kept string, last bool, err error) {
	ns, release := c.GetNamespace(), compose.ReleaseName(c.GetName(), pool)
	if last, err = lastPool(ctx, dyn, ns, c.GetName(), release); err != nil {
		return nil, nil, "", false, err
	}
	if last {
		operator := compose.OperatorReleaseName(c.GetName())
		targets = append(targets, objectRef{HelmReleaseGVR, ns, operator}, objectRef{compose.OCIRepositoryGVR, ns, operator})
		if slice, err = ownedSlice(ctx, dyn, ns, c.GetName()); err != nil {
			return nil, nil, "", false, err
		}
		switch {
		case slice != nil && sharesAnotherSlice(slice):
			// Another slice shares the release: it stays, serving and its
			// backend registration with it.
			kept = ns + "/" + slice.GetName()
			slice = nil
		default:
			removals, err := s.sliceRemovals(ctx, dyn, ns, c.GetName())
			if err != nil {
				return nil, nil, "", false, err
			}
			targets = append(targets, removals...)
		}
	}
	targets = append(targets,
		objectRef{compose.OCIRepositoryGVR, ns, release},
		objectRef{compose.SecretGVR, ns, compose.ValuesSecretName(c.GetName(), pool)},
		objectRef{HelmReleaseGVR, ns, release},
	)
	return targets, slice, kept, last, nil
}

// ownedSlice is the cluster's slice release when it exists and is
// cluster-manager's; nil otherwise.
func ownedSlice(ctx context.Context, dyn dynamic.Interface, ns, cluster string) (*unstructured.Unstructured, error) {
	name := compose.SliceReleaseName(cluster)
	hr, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get HelmRelease %s/%s: %w", ns, name, err)
	}
	if !compose.OwnedBy(hr) {
		return nil, nil
	}
	return hr, nil
}

// sharesAnotherSlice reports whether a slice release carries a slice besides
// serving, so it stays when model serving goes.
func sharesAnotherSlice(hr *unstructured.Unstructured) bool {
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	return compose.OtherSliceOn(values)
}

// objectRef names one object to delete.
type objectRef struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string
}

// lastPool reports whether release is the cluster's only remaining GPU pool
// release.
func lastPool(ctx context.Context, dyn dynamic.Interface, ns, cluster, release string) (bool, error) {
	pools, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: compose.LabelChartName + "=" + compose.PoolChart + "," + compose.LabelCluster + "=" + cluster,
	})
	if err != nil {
		return false, fmt.Errorf("list pool releases of %s: %w", cluster, err)
	}
	for i := range pools.Items {
		if pools.Items[i].GetName() != release {
			return false, nil
		}
	}
	return true, nil
}

// backendRegisteredFor reports whether model-manager's kserve backend
// document is the one cluster-manager wrote for cluster; another cluster's
// document is left alone.
func backendRegisteredFor(ctx context.Context, dyn dynamic.Interface, ns, cluster string) (bool, error) {
	cm, err := dyn.Resource(compose.ConfigMapGVR).Namespace(ns).Get(ctx, compose.BackendConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get ConfigMap %s/%s: %w", ns, compose.BackendConfigMapName, err)
	}
	return compose.OwnedBy(cm) && cm.GetLabels()[compose.LabelCluster] == cluster, nil
}

func checkMode(mode string) error {
	switch mode {
	case ModeApply:
		return nil
	case ModeCommit:
		return &ErrRefused{Reason: "mode commit (a pull request opened as you) is not available yet in this server version; use mode apply — the objects land on the installation as you and are removed with the cluster"}
	default:
		return &ErrRefused{Reason: fmt.Sprintf("mode %q: apply is the only mode this server version offers", mode)}
	}
}

// nodesGuard refuses while a node of the pool is busy — a pod with a GPU or
// a KServe predictor's runs on it, or its NodeClaim is still launching — and
// answers the pool's idle nodes, whose NodeClaims the teardown removes first.
// The nodes are read on the cluster as the caller (readPoolLive): Karpenter's
// NodeClaims of the pool, the Nodes registered from them, the pods on each.
// The MachinePool's provider IDs decide only when neither can be read — the
// list lags a terminated instance by minutes, and kept a delete refused for
// eight minutes after the node was gone (giantswarm/cluster-manager#49). The
// refusal names the busy nodes with what holds them and the models served on
// the cluster — read from the serving objects directly, so a predictor still
// Pending counts too — with the fix, and says what it read from. With no
// busy node the served models decide alone (waitingGuard): one whose
// predictor runs on no node waits for a node of the pool, and the delete is
// refused naming it (giantswarm/cluster-manager#59).
func (s *Service) nodesGuard(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, pool string, t target) ([]*poolNode, error) {
	ns := c.GetNamespace()
	mp, err := dyn.Resource(MachinePoolGVR).Namespace(ns).Get(ctx, pool, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get MachinePool %s/%s: %w", ns, pool, err)
	}
	if t.Reader == nil {
		return nil, machinePoolGuard(ctx, dyn, mp, t, t.Reason)
	}
	live := readPoolLive(ctx, t.Reader, pool, true)
	if !live.readable() {
		return nil, machinePoolGuard(ctx, dyn, mp, t, fmt.Sprintf("the pool's NodeClaims and Nodes on %s cannot be listed as you (%v; %v)", t.Cluster, live.claimsErr, live.nodesErr))
	}
	var busy []*poolNode
	var reasons []string
	for _, n := range live.nodes {
		if reason := live.holds(n); reason != "" {
			busy = append(busy, n)
			reasons = append(reasons, reason)
		}
	}
	idle := live.idle()
	if len(busy) == 0 {
		last, err := lastPool(ctx, dyn, ns, c.GetName(), pool)
		if err != nil {
			return nil, err
		}
		if err := s.waitingGuard(ctx, t, pool, last, idle); err != nil {
			return nil, err
		}
		return idle, nil
	}
	clause, models := servedModelsClause(ctx, t, true)
	reason := fmt.Sprintf("node pool %s still runs %d busy node(s) on %s: %s%s", pool, len(busy), t.Cluster, strings.Join(reasons, "; "), clause)
	if len(idle) > 0 {
		reason += fmt.Sprintf("; %d idle node(s) (%s) go with the pool once the busy ones are free", len(idle), strings.Join(nodeNames(idle), ", "))
	}
	return nil, &ErrRefused{
		Reason:  reason,
		Refused: &Refused{Nodes: nodeNames(busy), Idle: nodeNames(idle), Models: models, Hint: refusedHint, ReadFrom: readFromCluster},
	}
}

// machinePoolGuard is the nodes guard when the cluster's nodes cannot be
// read as the caller (why says so): the MachinePool's replicas and provider
// IDs decide — a list that lags the instances by minutes — and the refusal
// says they did.
func machinePoolGuard(ctx context.Context, dyn dynamic.Interface, mp *unstructured.Unstructured, t target, why string) error {
	replicas := nestedInt(mp, "spec", "replicas")
	if replicas == 0 {
		return nil
	}
	ids := machinePoolNodes(ctx, dyn, mp)
	clause, models := servedModelsClause(ctx, t, false)
	return &ErrRefused{
		Reason:  fmt.Sprintf("node pool %s still runs %d node(s) as its MachinePool lists them (%s) — %s, so the MachinePool's provider IDs decide, a list that lags a terminated instance by minutes%s", mp.GetName(), replicas, joinOrUnknown(ids), why, clause),
		Refused: &Refused{Nodes: ids, Models: models, Hint: refusedHintMachinePool, ReadFrom: readFromMachinePool},
	}
}

// machinePoolNodes names a MachinePool's nodes by provider id, from its
// infrastructure object; none when that cannot be read.
func machinePoolNodes(ctx context.Context, dyn dynamic.Interface, mp *unstructured.Unstructured) []string {
	ref := nestedRef(mp, "spec", "template", "spec", "infrastructureRef")
	if ref == nil {
		return []string{}
	}
	infra, err := getRef(ctx, dyn, ref, mp.GetNamespace())
	if err != nil {
		return []string{}
	}
	ids := providerIDs(infra)
	if ids == nil {
		return []string{}
	}
	return ids
}

// joinOrUnknown lists names for a message; "unknown" when there are none.
func joinOrUnknown(names []string) string {
	if len(names) == 0 {
		return "unknown"
	}
	return strings.Join(names, ", ")
}

// servedModelsClause is the nodes guard's second half: the models served on
// the cluster, named for unloading (a GPU pool's nodes carry their
// predictors); that none is, so something else holds the nodes — named
// before when the nodes were read live, else unknown —; or why they cannot
// be told — each with the fix.
func servedModelsClause(ctx context.Context, t target, live bool) (string, []string) {
	const rerun = "re-run once the pool is empty, or pass force to delete the pool with its nodes"
	if t.Reader == nil {
		return fmt.Sprintf("; whether models are served on %s cannot be told (%s) — check the cluster's Serving group, scale the workloads away and %s", t.Cluster, t.Reason, rerun), []string{}
	}
	models, err := detect.ServedModels(ctx, t.Reader)
	if err != nil {
		return fmt.Sprintf("; whether models are served on %s cannot be told (%v) — check the cluster's Serving group, scale the workloads away and %s", t.Cluster, err, rerun), []string{}
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.String())
	}
	switch {
	case len(models) == 0 && live:
		return fmt.Sprintf(" and %s serves no model: nothing of the platform's serving holds them — scale what is named away and %s", t.Cluster, rerun), names
	case len(models) == 0:
		return fmt.Sprintf(" and %s serves no model: nothing of the platform's serving holds them — something else is scheduled there, or Karpenter has not consolidated the empty node yet — %s", t.Cluster, rerun), names
	}
	return fmt.Sprintf(", serving %d model(s) on %s: %s — unload them first (model-manager's unload_model, or the cluster's Serving group) and %s and the models on them", len(models), t.Cluster, joinModels(models), rerun), names
}

// clusterFacts reads what the pool release needs: the pins from the
// cluster's Release CR, the credential-free snapshot from the cluster's
// values.
func (s *Service) clusterFacts(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (compose.Cluster, error) {
	facts := s.identity(c)
	if err := s.releasePins(ctx, dyn, c, &facts); err != nil {
		return facts, err
	}
	vals, err := clusterValues(ctx, dyn, c)
	if err != nil {
		return facts, err
	}
	facts.BaseDomain, _, _ = unstructured.NestedString(vals, "global", "connectivity", "baseDomain")
	if facts.BaseDomain == "" {
		return facts, fmt.Errorf("values of cluster %s carry no global.connectivity.baseDomain: the pool's bootstrap needs the installation's base domain", c.GetName())
	}
	facts.ManagementCluster, _, _ = unstructured.NestedString(vals, "global", "managementCluster")
	if facts.ManagementCluster == "" {
		facts.ManagementCluster = s.cfg.Installation
	}
	facts.CiliumIPAMMode, _, _ = unstructured.NestedString(vals, "global", "connectivity", "cilium", "ipamMode")
	facts.RegistryMirrors, facts.RegistryCredentials = registries(vals)
	facts.Proxy = proxy(vals)
	return facts, nil
}

// releasePins sets the Kubernetes version and machine image from the
// cluster's Release CR.
func (s *Service) releasePins(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, facts *compose.Cluster) error {
	version := c.GetLabels()[LabelReleaseVersion]
	if version == "" {
		return fmt.Errorf("cluster %s carries no %s label: the pool's pins come from the cluster's release", c.GetName(), LabelReleaseVersion)
	}
	release, err := findRelease(ctx, dyn, version)
	if err != nil {
		return err
	}
	components := releaseComponents(release)
	for _, name := range []string{releaseComponentKubernetes, releaseComponentFlatcar, releaseComponentOSTooling} {
		if components[name] == "" {
			return fmt.Errorf("release %s names no %s component: the pool's pins come from the cluster's release", release.GetName(), name)
		}
	}
	facts.KubernetesVersion = components[releaseComponentKubernetes]
	facts.MachineImage = fmt.Sprintf("flatcar-%s-%s-kube-%s-tooling-%s-gs", osReleaseChannel, components[releaseComponentFlatcar], components[releaseComponentKubernetes], components[releaseComponentOSTooling])
	return nil
}

// findRelease is the Release CR of a release version (`<provider>-<version>`).
func findRelease(ctx context.Context, dyn dynamic.Interface, version string) (*unstructured.Unstructured, error) {
	releases, err := dyn.Resource(ReleaseGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	for i := range releases.Items {
		if strings.HasSuffix(releases.Items[i].GetName(), "-"+version) {
			return &releases.Items[i], nil
		}
	}
	return nil, &ErrNotFound{What: "release " + version + " (Release CR)"}
}

// releaseComponents is component name → version without the v prefix.
func releaseComponents(release *unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	components, _, _ := unstructured.NestedSlice(release.Object, "spec", "components")
	for _, comp := range components {
		m, ok := comp.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		version, _ := m["version"].(string)
		out[name] = strings.TrimPrefix(version, "v")
	}
	return out
}

// clusterValues is the cluster's own values: the fleet's HelmRelease named
// like the cluster (spec.valuesFrom ConfigMaps under spec.values, Flux's
// precedence), else the App CR of that name (its user-values ConfigMap).
// Secrets referenced from either are not read: their content is credentials.
func clusterValues(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (map[string]any, error) {
	ns, name := c.GetNamespace(), c.GetName()
	if hr, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return helmReleaseValues(ctx, dyn, hr)
	}
	if app, err := dyn.Resource(AppGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		cmName, _, _ := unstructured.NestedString(app.Object, "spec", "userConfig", "configMap", "name")
		cmNS, _, _ := unstructured.NestedString(app.Object, "spec", "userConfig", "configMap", "namespace")
		if cmNS == "" {
			cmNS = ns
		}
		if cmName == "" {
			return nil, fmt.Errorf("the App %s/%s names no user-values ConfigMap: the pool's snapshot comes from the cluster's values", ns, name)
		}
		return configMapValues(ctx, dyn, cmNS, cmName, "values")
	}
	return nil, &ErrNotFound{What: fmt.Sprintf("values of cluster %s (neither a HelmRelease nor an App named %s in %s)", name, name, ns)}
}

// helmReleaseValues merges a HelmRelease's values the way Flux does: the
// valuesFrom ConfigMaps in order, the inline spec.values on top. Secrets
// among the valuesFrom are not read: their content is credentials.
func helmReleaseValues(ctx context.Context, dyn dynamic.Interface, hr *unstructured.Unstructured) (map[string]any, error) {
	merged := map[string]any{}
	refs, _, _ := unstructured.NestedSlice(hr.Object, "spec", "valuesFrom")
	for _, r := range refs {
		ref, _ := r.(map[string]any)
		if ref["kind"] != "ConfigMap" {
			continue
		}
		key, _ := ref["valuesKey"].(string)
		if key == "" {
			key = "values.yaml"
		}
		cmName, _ := ref["name"].(string)
		vals, err := configMapValues(ctx, dyn, hr.GetNamespace(), cmName, key)
		if err != nil {
			return nil, err
		}
		merge(merged, vals)
	}
	inline, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	merge(merged, inline)
	return merged, nil
}

func configMapValues(ctx context.Context, dyn dynamic.Interface, ns, name, key string) (map[string]any, error) {
	cm, err := dyn.Resource(ConfigMapGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get values ConfigMap %s/%s: %w", ns, name, err)
	}
	raw, _, _ := unstructured.NestedString(cm.Object, "data", key)
	vals := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &vals); err != nil {
		return nil, fmt.Errorf("values ConfigMap %s/%s key %s: %w", ns, name, key, err)
	}
	return vals, nil
}

// merge overlays src onto dst, maps recursively.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				merge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// registries splits global.components.containerd.containerRegistries into
// the credential-free mirrors (host → endpoints) and the credentials
// (endpoint → credential).
func registries(vals map[string]any) (map[string][]string, map[string]compose.RegistryCredential) {
	mirrors := map[string][]string{}
	creds := map[string]compose.RegistryCredential{}
	regs, _, _ := unstructured.NestedMap(vals, "global", "components", "containerd", "containerRegistries")
	for host, v := range regs {
		entries, _ := v.([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			endpoint, _ := entry["endpoint"].(string)
			if endpoint == "" {
				continue
			}
			mirrors[host] = append(mirrors[host], endpoint)
			if cred, ok := entry["credentials"].(map[string]any); ok && len(cred) > 0 {
				user, _ := cred["username"].(string)
				pass, _ := cred["password"].(string)
				auth, _ := cred["auth"].(string)
				creds[endpoint] = compose.RegistryCredential{Username: user, Password: pass, Auth: auth}
			}
		}
	}
	return mirrors, creds
}

// proxy is global.connectivity.proxy; noProxy is taken as the values state
// it (a string, or a list joined by commas).
func proxy(vals map[string]any) compose.Proxy {
	p, _, _ := unstructured.NestedMap(vals, "global", "connectivity", "proxy")
	enabled, _ := p["enabled"].(bool)
	if !enabled {
		return compose.Proxy{}
	}
	out := compose.Proxy{Enabled: true}
	out.HTTPProxy, _ = p["httpProxy"].(string)
	out.HTTPSProxy, _ = p["httpsProxy"].(string)
	switch np := p["noProxy"].(type) {
	case string:
		out.NoProxy = np
	case []any:
		parts := make([]string, 0, len(np))
		for _, x := range np {
			parts = append(parts, fmt.Sprint(x))
		}
		out.NoProxy = strings.Join(parts, ",")
	case map[string]any:
		if addrs, ok := np["addresses"].([]any); ok {
			parts := make([]string, 0, len(addrs))
			for _, x := range addrs {
				parts = append(parts, fmt.Sprint(x))
			}
			out.NoProxy = strings.Join(parts, ",")
		}
	}
	return out
}

// The actions of an apply.
const (
	actionCreate    = "create"
	actionUpdate    = "update"
	actionUnchanged = "unchanged"
	actionDelete    = "delete"
	actionPending   = "pending"
)

// writeReserve is the least of the budget a write may start with: about to
// start with less, it is left pending and the answer goes out partial, in
// time, instead of the call being cancelled mid-write by the caller's
// deadline (giantswarm/cluster-manager#34).
const writeReserve = time.Second

// applyAll lands the composed objects as the caller. First every object is
// read and its action decided — concurrently, and every refusal before any
// write: apply mode lands new objects only and never patches an object
// someone else owns. Then the writes go one at a time in the objects' order,
// the objects that do not exist yet before the updates, so a call that does
// not reach the end leaves what a re-run completes first. A write about to
// start with less than writeReserve of the budget left is not started: the
// answer marks it and the rest pending, Partial, with the re-run as the next
// step. Dry-run touches nothing and reports what would happen. The answer's
// objects keep the composed order.
func applyAll(ctx context.Context, dyn dynamic.Interface, objs []*unstructured.Unstructured, dryRun bool, out *WriteResult, budget time.Time) error {
	defer timed(ctx, "apply", "objects", len(objs), "dryRun", dryRun)()
	plans := make([]applyPlan, len(objs))
	g, gctx := errgroup.WithContext(ctx)
	for i, obj := range objs {
		g.Go(func() (err error) {
			plans[i], err = planApply(gctx, dyn, obj)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	first := len(out.Objects)
	for i := range plans {
		act := plans[i].act
		if dryRun && act.Action != actionUnchanged {
			act.Action = "would-" + act.Action
		}
		out.Objects = append(out.Objects, act)
		out.Manifests = append(out.Manifests, redacted(objs[i]))
	}
	if dryRun {
		return nil
	}
	var writes []int
	for _, action := range []string{actionCreate, actionUpdate} {
		for i := range plans {
			if plans[i].act.Action == action {
				writes = append(writes, i)
			}
		}
	}
	for n, i := range writes {
		if remaining := time.Until(budget); remaining < writeReserve {
			out.Partial = true
			for _, j := range writes[n:] {
				out.Objects[first+j].Action = actionPending
			}
			out.NextStep = fmt.Sprintf("%d of %d object(s) are pending: the answer went out within the caller's deadline instead of starting them — re-run with the same arguments, the pending objects are written first", len(writes)-n, len(objs))
			slog.WarnContext(ctx, "apply cut short: the remaining budget is below the write reserve", "remaining", remaining.Round(time.Millisecond), "reserve", writeReserve, "pending", len(writes)-n)
			return nil
		}
		if err := plans[i].write(ctx); err != nil {
			return err
		}
	}
	return nil
}

// applyPlan is one composed object against its live counterpart: what apply
// does with it.
type applyPlan struct {
	obj      *unstructured.Unstructured
	existing *unstructured.Unstructured
	res      dynamic.ResourceInterface
	act      ObjectAction
}

// planApply reads the live object and decides: create when absent, update or
// unchanged when cluster-manager created it, refused when someone else owns
// it.
func planApply(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) (applyPlan, error) {
	gvr, err := refGVR(obj.GetAPIVersion(), obj.GetKind())
	if err != nil {
		return applyPlan{}, err
	}
	p := applyPlan{obj: obj, res: dyn.Resource(gvr).Namespace(obj.GetNamespace()), act: ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace()}}
	existing, err := p.res.Get(ctx, obj.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		p.act.Action = actionCreate
	case err != nil:
		return p, fmt.Errorf("get %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	case !compose.OwnedBy(existing):
		return p, &ErrRefused{Reason: fmt.Sprintf("%s %s/%s exists and %s: apply mode lands new objects only and never patches an object someone else owns — %s", obj.GetKind(), obj.GetNamespace(), obj.GetName(), ownerDescription(existing), removalHint(existing))}
	default:
		p.existing = existing
		p.act.Changes = changedPaths(existing, obj)
		p.act.Action = actionUpdate
		if len(p.act.Changes) == 0 {
			p.act.Action = actionUnchanged
		}
	}
	return p, nil
}

// write lands the planned create or update, timed in the log at debug.
func (p *applyPlan) write(ctx context.Context) error {
	defer timed(ctx, "write", "action", p.act.Action, "kind", p.act.Kind, "name", p.act.Namespace+"/"+p.act.Name)()
	switch p.act.Action {
	case actionCreate:
		if _, err := p.res.Create(ctx, p.obj, metav1.CreateOptions{FieldManager: compose.ManagedBy}); err != nil {
			return fmt.Errorf("create %s %s/%s: %w", p.obj.GetKind(), p.obj.GetNamespace(), p.obj.GetName(), err)
		}
	case actionUpdate:
		p.obj.SetResourceVersion(p.existing.GetResourceVersion())
		if _, err := p.res.Update(ctx, p.obj, metav1.UpdateOptions{FieldManager: compose.ManagedBy}); err != nil {
			return fmt.Errorf("update %s %s/%s: %w", p.obj.GetKind(), p.obj.GetNamespace(), p.obj.GetName(), err)
		}
	}
	return nil
}

// deleteIfOwned deletes one of cluster-manager's objects; nil when it does
// not exist. An object of that name someone else owns is left alone.
func deleteIfOwned(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string, dryRun bool) (*ObjectAction, error) {
	res := dyn.Resource(gvr).Namespace(ns)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvr.Resource, ns, name, err)
	}
	if !compose.OwnedBy(obj) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("%s %s/%s %s: cluster-manager removes only what it created — %s", obj.GetKind(), ns, name, ownerDescription(obj), removalHint(obj))}
	}
	act := &ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: name, Namespace: ns, Action: "delete"}
	if dryRun {
		act.Action = "would-delete"
		return act, nil
	}
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), ns, name, err)
	}
	return act, nil
}

// ownerDescription says who owns an object that is not cluster-manager's.
func ownerDescription(obj *unstructured.Unstructured) string {
	labels := obj.GetLabels()
	if k := labels[labelKustomizeName]; k != "" {
		return "is owned by GitOps (Flux Kustomization " + k + ")"
	}
	if m := labels[compose.LabelManagedBy]; m != "" {
		return "is managed by " + m
	}
	return "was not created by cluster-manager"
}

// removalHint points at the way to change an object cluster-manager does not
// own: the git repository for a GitOps-owned one, else its owner.
func removalHint(obj *unstructured.Unstructured) string {
	if obj.GetLabels()[labelKustomizeName] != "" {
		return "change it in the git repository that owns the cluster (mode commit, once available), or pick another pool name"
	}
	return "remove it by the means that created it, or pick another pool name"
}

// serverDefaults are the spec leaves the API server fills in from the CRD's
// defaults on every create and update, by the composed object's apiVersion
// and kind. The composed objects leave them unset, so the live object always
// carries them and an update always gets them back: a leaf only the live
// object carries, at its default value, is not drift. A live value other than
// the default is — the update would reset it. Flux's OCIRepository v1 defaults
// `spec.provider` and `spec.timeout` (source.toolkit.fluxcd.io/v1
// OCIRepositorySpec); HelmRelease v2 defaults nothing at the paths
// cluster-manager composes, nor do Secret and ConfigMap.
var serverDefaults = map[string]map[string]any{
	compose.OCIRepositoryGVR.GroupVersion().String() + "/OCIRepository": {
		"spec.provider": "generic",
		"spec.timeout":  "60s",
	},
}

// changedPaths lists the spec, label and ownerReference paths of want that
// differ from have — the drift a re-run would correct. A leaf the API server
// defaulted on have and want leaves unset is no difference (serverDefaults).
func changedPaths(have, want *unstructured.Unstructured) []string {
	var paths []string
	diff(have.Object["spec"], want.Object["spec"], "spec", &paths)
	diff(have.Object["stringData"], want.Object["stringData"], "stringData", &paths)
	diff(have.Object["data"], want.Object["data"], "data", &paths)
	diff(map[string]any{"labels": have.GetLabels()}, map[string]any{"labels": want.GetLabels()}, "metadata", &paths)
	if !reflect.DeepEqual(have.GetOwnerReferences(), want.GetOwnerReferences()) {
		paths = append(paths, "metadata.ownerReferences")
	}
	defaults := serverDefaults[want.GetAPIVersion()+"/"+want.GetKind()]
	out := paths[:0]
	for _, p := range paths {
		if !serverDefaulted(defaults, p, have, want) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// serverDefaulted reports whether path is a leaf want leaves unset that the
// API server filled in on have with its default value.
func serverDefaulted(defaults map[string]any, path string, have, want *unstructured.Unstructured) bool {
	def, ok := defaults[path]
	if !ok {
		return false
	}
	fields := strings.Split(path, ".")
	if _, set, _ := unstructured.NestedFieldNoCopy(want.Object, fields...); set {
		return false
	}
	got, set, _ := unstructured.NestedFieldNoCopy(have.Object, fields...)
	return set && equalLeaf(got, def)
}

// diff walks maps side by side and records every leaf path whose values
// differ (numbers compared by value, whatever their Go type); a subtree one
// side lacks is walked as empty, so the paths name the leaves.
func diff(have, want any, path string, out *[]string) {
	hm, hok := have.(map[string]any)
	wm, wok := want.(map[string]any)
	if hok && want == nil {
		wm, wok = map[string]any{}, true
	}
	if wok && have == nil {
		hm, hok = map[string]any{}, true
	}
	if hok && wok {
		keys := map[string]bool{}
		for k := range hm {
			keys[k] = true
		}
		for k := range wm {
			keys[k] = true
		}
		for k := range keys {
			diff(hm[k], wm[k], path+"."+k, out)
		}
		return
	}
	if !equalLeaf(have, want) {
		*out = append(*out, path)
	}
}

func equalLeaf(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	na, aok := number(a)
	nb, bok := number(b)
	return aok && bok && na == nb
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// versionNewer reports whether a (`1.31.4`) is newer than b; false with an
// error when either does not parse (an unknown control plane version does
// not block — the controller's own skew check does).
func versionNewer(a, b string) (bool, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i], nil
		}
	}
	return false, nil
}

func parseVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return out, errors.New("not a three-part version: " + v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.SplitN(p, "-", 2)[0])
		if err != nil {
			return out, fmt.Errorf("version %s: %w", v, err)
		}
		out[i] = n
	}
	return out, nil
}

// redacted is the manifest as the tool returns it: a Secret's data replaced
// by the keys it carries.
func redacted(obj *unstructured.Unstructured) map[string]any {
	if obj.GetKind() != "Secret" {
		return obj.Object
	}
	cp := obj.DeepCopy()
	data, _, _ := unstructured.NestedMap(cp.Object, "stringData")
	keys := map[string]any{}
	for k := range data {
		keys[k] = "<redacted>"
	}
	cp.Object["stringData"] = keys
	return cp.Object
}
