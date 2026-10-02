package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// helmManagedBy is LabelManagedBy's value on the objects a Helm release
// renders: the cluster release's default apps, and the releases those
// render in turn.
const helmManagedBy = "Helm"

// DeleteClusterInput is delete_cluster's input.
type DeleteClusterInput struct {
	Organization string
	Name         string
	Mode         string
	DryRun       bool
}

// DeleteCluster removes a workload cluster create_cluster created, as the
// caller, in two passes over the same arguments.
//
// While the cluster's HelmRelease exists, every refusal comes before any
// write: a release cluster-manager did not create, the installation's own
// cluster, a release in a Flux Kustomization's inventory (labels alone do not
// decide: a removal merged into a repository whose Kustomization does not
// prune leaves the objects out of the inventory, and apply may delete them),
// and a release helm-controller has not reconciled since its last write —
// without its finalizer the delete would not uninstall the cluster. Then the
// kserve backend registration cluster-manager wrote for the cluster goes, the
// HelmRelease — helm-controller uninstalls the cluster, its default apps with
// it, and the Cluster's garbage collection takes the GPU pool, operator and
// slice releases that carry an ownerReference to it —, its OCIRepository and
// its values ConfigMap. The answer lists those releases and the models served
// on the cluster, and nextStep names the second pass.
//
// Once the HelmRelease is gone and the uninstall has removed the Cluster, the
// same call removes what the uninstall left in the organization's namespace
// (giantswarm/cluster-manager#132's proof): the HelmReleases and
// OCIRepositories of the cluster rendered by Helm or by cluster-manager — a
// default app whose install was still running when the cluster went — and
// cluster-manager's own source and values. While the Cluster is still being
// removed it writes nothing and says so.
//
// Mode commit opens the removal pull request in the repository that owns the
// organization instead (commitDeleteCluster). Where that repository's
// Kustomization does not prune, the merge leaves the cluster's own
// Kustomization on the installation, out of the inventory: the first pass in
// mode apply then deletes that Kustomization, and Flux removes what it applied.
func (s *Service) DeleteCluster(ctx context.Context, in DeleteClusterInput) (*WriteResult, error) {
	start := time.Now()
	if err := s.checkMode(in.Mode, true); err != nil {
		return nil, err
	}
	if in.Organization == "" {
		return nil, &ErrRefused{Reason: "organization: required, the cluster lives in its org- namespace"}
	}
	if !compose.ClusterNamePattern.MatchString(in.Name) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("cluster name %q: not a name create_cluster gives (a DNS label of at most %d characters starting with a letter)", in.Name, compose.MaxClusterNameLength)}
	}
	spec := compose.ClusterSpec{Organization: in.Organization, Name: in.Name}
	ns := spec.Namespace()
	if s.cfg.Installation != "" && in.Name == s.cfg.Installation {
		return nil, &ErrRefused{Reason: fmt.Sprintf("cluster %s is the installation's own cluster (its management cluster): delete_cluster never removes it", in.Name)}
	}
	k := s.clients(ctx)
	if err := requireClusterAPI(k.Discovery, ns+"/"+in.Name); err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	cluster, err := getOptional(ctx, dyn, ClusterGVR, ns, in.Name)
	if err != nil {
		return nil, err
	}
	hr, err := getOptional(ctx, dyn, HelmReleaseGVR, ns, in.Name)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{Cluster: in.Name, Namespace: ns, Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{}}
	if in.Mode == ModeCommit {
		if err := s.commitDeleteCluster(ctx, dyn, cluster, hr, in, out, start); err != nil {
			return nil, err
		}
		logApplied(ctx, "delete_cluster", out, start)
		return out, nil
	}
	if hr == nil {
		if err := s.deleteLeftovers(ctx, dyn, cluster, in, out, start); err != nil {
			return nil, err
		}
		logApplied(ctx, "delete_cluster", out, start)
		return out, nil
	}
	if !compose.OwnedBy(hr) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("HelmRelease %s/%s %s: delete_cluster removes only a cluster create_cluster created — delete it the way it was made", ns, in.Name, ownerDescription(hr))}
	}
	ks, err := inGit(ctx, dyn, hr)
	if err != nil {
		return nil, err
	}
	var merged *unstructured.Unstructured
	if ks != "" {
		if merged, err = s.mergedRemoval(ctx, dyn, ks, in.Name); err != nil {
			return nil, err
		}
		if merged == nil {
			return nil, &ErrRefused{Reason: fmt.Sprintf("HelmRelease %s/%s is in git: Flux Kustomization %s applies it, so a live delete would be undone — remove the cluster from that repository (mode commit), and after the merge, where the Kustomization does not prune, with mode apply", ns, in.Name, ks)}
		}
	}
	// Without helm-controller's finalizer the delete removes the HelmRelease
	// at once and nothing uninstalls the cluster: its Cluster API objects and
	// cloud resources would stay.
	if hr.GetDeletionTimestamp() == nil && !slices.Contains(hr.GetFinalizers(), fluxFinalizer) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("HelmRelease %s/%s carries no %s: helm-controller has not reconciled it since its last write, and deleting it now removes it without uninstalling the cluster — its Cluster and cloud resources would stay; re-run once helm-controller has reconciled it (within its interval, or at once with `flux reconcile helmrelease -n %s %s`)", ns, in.Name, fluxFinalizer, ns, in.Name)}
	}
	var targets []objectRef
	registered, err := backendRegisteredFor(ctx, dyn, s.cfg.ModelManagerNamespace, in.Name)
	if err != nil {
		return nil, err
	}
	if registered {
		targets = append(targets, objectRef{compose.ConfigMapGVR, s.cfg.ModelManagerNamespace, compose.BackendConfigMapName})
	}
	if merged == nil {
		targets = append(targets,
			objectRef{HelmReleaseGVR, ns, in.Name},
			objectRef{compose.OCIRepositoryGVR, ns, in.Name},
			objectRef{ConfigMapGVR, ns, compose.ClusterValuesName(in.Name)},
		)
	}
	plans, err := planDeletes(ctx, dyn, targets)
	if err != nil {
		return nil, err
	}
	// The cluster's own Kustomization, out of the inventory once its removal
	// was merged: deleting it, Flux prunes the release, its source and its
	// values, and the release's uninstall removes the cluster.
	if merged != nil {
		plans = append(plans, deletePlan{res: dyn.Resource(KustomizationGVR).Namespace(merged.GetNamespace()), act: actionOf(merged)})
	}
	if out.WithCluster, err = clusterReleases(ctx, dyn, ns, in.Name); err != nil {
		return nil, err
	}
	if cluster != nil {
		out.Models, out.ModelsNote = servedModels(ctx, s.target(ctx, dyn, cluster))
	}
	td := newTeardown(ctx, dyn, in.DryRun, out, s.budget(ctx, start))
	if err := td.deleteAll(plans); err != nil {
		return nil, err
	}
	td.finish()
	if !out.Partial {
		out.NextStep = fmt.Sprintf("helm-controller uninstalls the cluster — a small one is gone in about five minutes, list_clusters lists it until then —; re-run delete_cluster with the same arguments afterwards: it removes what the uninstall left in %s (a default app whose install was still running)", ns)
	}
	logApplied(ctx, "delete_cluster", out, start)
	return out, nil
}

// deleteLeftovers is delete_cluster's second pass, the cluster's HelmRelease
// gone: nothing while the Cluster is still being removed; a Cluster that is
// not being removed was not made by create_cluster; with the Cluster gone,
// the cluster's HelmReleases and OCIRepositories the uninstall left —
// rendered by Helm or by cluster-manager, labelled with the cluster and
// named after it — and cluster-manager's own source and values. Another
// owner's object is named and left; one in a Kustomization's inventory
// refuses the pass before any write.
func (s *Service) deleteLeftovers(ctx context.Context, dyn dynamic.Interface, cluster *unstructured.Unstructured, in DeleteClusterInput, out *WriteResult, start time.Time) error {
	ns := out.Namespace
	switch {
	case cluster != nil && cluster.GetDeletionTimestamp() == nil:
		return &ErrRefused{Reason: fmt.Sprintf("cluster %s/%s has no HelmRelease %s/%s: it was not created by create_cluster — delete it the way it was made", ns, in.Name, ns, in.Name)}
	case cluster != nil:
		out.NextStep = fmt.Sprintf("cluster %s/%s is being removed since %s: helm-controller's uninstall is still running — re-run delete_cluster with the same arguments once list_clusters no longer lists it", ns, in.Name, cluster.GetDeletionTimestamp().UTC().Format(time.RFC3339))
		return nil
	}
	var plans []deletePlan
	for _, gvr := range []schema.GroupVersionResource{HelmReleaseGVR, compose.OCIRepositoryGVR} {
		found, err := leftovers(ctx, dyn, gvr, ns, in.Name, out)
		if err != nil {
			return err
		}
		plans = append(plans, found...)
	}
	values, err := getOptional(ctx, dyn, ConfigMapGVR, ns, compose.ClusterValuesName(in.Name))
	if err != nil {
		return err
	}
	if values != nil && compose.OwnedBy(values) {
		plans = append(plans, deletePlan{res: dyn.Resource(ConfigMapGVR).Namespace(ns), act: actionOf(values)})
	}
	if len(plans) == 0 && len(out.Warnings) == 0 {
		return &ErrNotFound{What: fmt.Sprintf("cluster %s/%s (nothing of it is left)", ns, in.Name)}
	}
	td := newTeardown(ctx, dyn, in.DryRun, out, s.budget(ctx, start))
	if err := td.deleteAll(plans); err != nil {
		return err
	}
	td.finish()
	return nil
}

// leftovers plans the delete of the objects of one kind the cluster's
// uninstall left in ns: labelled with the cluster, named after it, rendered
// by Helm or by cluster-manager. Another owner's object is named in the
// answer's warnings and left; one a Kustomization applies is a refusal.
func leftovers(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, cluster string, out *WriteResult) ([]deletePlan, error) {
	res := dyn.Resource(gvr).Namespace(ns)
	list, err := res.List(ctx, metav1.ListOptions{LabelSelector: compose.LabelCluster + "=" + cluster})
	if err != nil {
		return nil, fmt.Errorf("list %s of cluster %s in %s: %w", gvr.Resource, cluster, ns, err)
	}
	var plans []deletePlan
	for i := range list.Items {
		obj := &list.Items[i]
		if obj.GetName() != cluster && !strings.HasPrefix(obj.GetName(), cluster+"-") {
			continue
		}
		if m := obj.GetLabels()[compose.LabelManagedBy]; m != compose.ManagedBy && m != helmManagedBy {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s %s/%s %s and is left: remove it by the means that created it", obj.GetKind(), ns, obj.GetName(), ownerDescription(obj)))
			continue
		}
		if ks, err := inGit(ctx, dyn, obj); err != nil {
			return nil, err
		} else if ks != "" {
			return nil, &ErrRefused{Reason: fmt.Sprintf("%s %s/%s is in git: Flux Kustomization %s applies it, so a live delete would be undone — remove it from that repository", obj.GetKind(), ns, obj.GetName(), ks)}
		}
		plans = append(plans, deletePlan{res: res, act: actionOf(obj)})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].act.Name < plans[j].act.Name })
	return plans, nil
}

// clusterReleases names the HelmReleases that go with the cluster's release:
// the GPU pool, operator and slice releases cluster-manager created, through
// their ownerReference to the Cluster, and the default apps the uninstall
// removes — every release in ns labelled with the cluster but its own.
func clusterReleases(ctx context.Context, dyn dynamic.Interface, ns, cluster string) ([]string, error) {
	list, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: compose.LabelCluster + "=" + cluster})
	if err != nil {
		return nil, fmt.Errorf("list the releases of cluster %s in %s: %w", cluster, ns, err)
	}
	var names []string
	for _, hr := range list.Items {
		if hr.GetName() != cluster {
			names = append(names, ns+"/"+hr.GetName())
		}
	}
	sort.Strings(names)
	return names, nil
}

// servedModels names the models served on the target, or why they cannot be
// told.
func servedModels(ctx context.Context, t target) ([]string, string) {
	if t.Reader == nil {
		return nil, fmt.Sprintf("the models served on %s cannot be told: %s", t.Cluster, t.Reason)
	}
	models, err := detect.ServedModels(ctx, t.Reader)
	if err != nil {
		return nil, fmt.Sprintf("the models served on %s cannot be told: %v", t.Cluster, err)
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.String())
	}
	return names, ""
}

// getOptional gets one object; nil when it does not exist.
func getOptional(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	obj, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvr.Resource, ns, name, err)
	}
	return obj, nil
}

// actionOf is the delete of one live object.
func actionOf(obj *unstructured.Unstructured) ObjectAction {
	return ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace()}
}

// mergedRemoval is the cluster's own Flux Kustomization ks (namespace/name)
// after its removal was merged into a repository whose Kustomization does not
// prune: named <installation>-clusters-<cluster> as create_cluster's commit
// writes it, pruning, applied by a Kustomization whose inventory no longer
// lists it. Nil for any other Kustomization: the release is still in git.
func (s *Service) mergedRemoval(ctx context.Context, dyn dynamic.Interface, ks, cluster string) (*unstructured.Unstructured, error) {
	namespace, name, _ := strings.Cut(ks, "/")
	if s.cfg.Installation == "" || name != s.cfg.Installation+"-clusters-"+cluster {
		return nil, nil
	}
	k, err := getOptional(ctx, dyn, KustomizationGVR, namespace, name)
	if err != nil || k == nil {
		return nil, err
	}
	if prune, _, _ := unstructured.NestedBool(k.Object, "spec", "prune"); !prune || k.GetLabels()[labelKustomizeName] == "" {
		return nil, nil
	}
	owner, err := inGit(ctx, dyn, k)
	if err != nil || owner != "" {
		return nil, err
	}
	return k, nil
}
