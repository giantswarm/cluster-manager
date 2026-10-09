package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/gitops-commit/layout"
	"github.com/giantswarm/gitops-commit/provenance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// commitCreateCluster lands create_cluster's objects as the pull request
// opened as the caller in the repository that owns the cluster's
// Organization (gitops-commit's new-cluster layout): one file per object in
// the cluster's own directory, the cluster's Flux Kustomization and its
// entry in the organization's workload-clusters/kustomization.yaml.
func (s *Service) commitCreateCluster(ctx context.Context, dyn dynamic.Interface, spec compose.ClusterSpec, objs []*unstructured.Unstructured, dryRun bool, out *WriteResult) error {
	place, err := clusterPlace(ctx, dyn, s.cfg.Installation, spec.Organization, spec.Name)
	if err != nil {
		return err
	}
	remote, gh, err := s.gitHubFor(ctx)
	if err != nil {
		return err
	}
	files, err := releaseFiles(place.Directory(), objs)
	if err != nil {
		return err
	}
	built, err := layout.BuildCluster(ctx, remote, place, files)
	if err != nil {
		return clusterLayoutError(err)
	}
	plan := &commitPlan{Plan: built.Plan, target: &CommitTarget{
		Repository: place.Repository.String(), Branch: place.Branch, Path: place.Directory().Path(),
		Kustomization: place.Owner.Namespace + "/" + place.Owner.Name, Prune: built.Prune,
	}, remote: remote}
	for _, obj := range objs {
		out.Manifests = append(out.Manifests, redacted(obj))
	}
	title := fmt.Sprintf("feat(%s): add workload cluster %s", spec.Organization, spec.Name)
	what := fmt.Sprintf("Adds the workload cluster `%s` to the organization `%s` (release %s), applied by its own Flux Kustomization `%s`", spec.Name, spec.Organization, spec.Release, built.Kustomization)
	res, err := plan.open(ctx, gh, "cluster-manager/create-cluster-"+spec.Name, title, commitBody(what, "create_cluster", gh.Login, plan, objs), dryRun)
	if err != nil {
		return err
	}
	out.Commit = res
	out.NextStep = commitNextStep(plan, res, dryRun, "the cluster", gh.Login, fmt.Sprintf("Flux Kustomization %s applies the cluster's Kustomization %s, which creates the cluster within its interval, and list_clusters shows it — create_node_pool in mode commit then writes its pools under %s/%s/", plan.target.Kustomization, built.Kustomization, plan.target.Path, CommitDirectory))
	return nil
}

// clusterPlace follows the Organization to the repository that owns it: the
// Flux Kustomization labels on the Organization CR, then that
// Kustomization's GitRepository. Read as the caller. An Organization no
// Kustomization reconciles is a refusal with the way out.
func clusterPlace(ctx context.Context, dyn dynamic.Interface, installation, organization, name string) (layout.Cluster, error) {
	org, err := dyn.Resource(OrganizationGVR).Get(ctx, organization, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return layout.Cluster{}, &ErrRefused{Reason: fmt.Sprintf("organization %s does not exist on this installation — create the organization first", organization)}
	}
	if err != nil {
		return layout.Cluster{}, fmt.Errorf("get Organization %s: %w", organization, err)
	}
	ksName, ksNamespace := org.GetLabels()[labelKustomizeName], org.GetLabels()[labelKustomizeNamespace]
	var flux provenance.Flux
	if ksName != "" {
		ks, err := dyn.Resource(KustomizationGVR).Namespace(ksNamespace).Get(ctx, ksName, metav1.GetOptions{})
		if err != nil {
			return layout.Cluster{}, fmt.Errorf("get Kustomization %s/%s owning organization %s: %w", ksNamespace, ksName, organization, err)
		}
		if err := flux.Add(ks.Object); err != nil {
			return layout.Cluster{}, err
		}
		if src := flux.Kustomizations[0].SourceRef; src.Kind == provenance.KindGitRepository {
			srcNamespace := src.Namespace
			if srcNamespace == "" {
				srcNamespace = ksNamespace
			}
			repo, err := dyn.Resource(GitRepositoryGVR).Namespace(srcNamespace).Get(ctx, src.Name, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return layout.Cluster{}, fmt.Errorf("get GitRepository %s/%s: %w", srcNamespace, src.Name, err)
			}
			if err == nil {
				if err := flux.Add(repo.Object); err != nil {
					return layout.Cluster{}, err
				}
			}
		}
	}
	place, err := layout.NewCluster(flux, ksNamespace, ksName, installation, organization, name)
	if err != nil {
		return layout.Cluster{}, clusterLayoutError(err)
	}
	return place, nil
}

// clusterLayoutError answers the layout's refusals with the way out.
func clusterLayoutError(err error) error {
	var secret *layout.SecretError
	switch {
	case errors.Is(err, layout.ErrNotReconciled):
		return &ErrRefused{Reason: err.Error() + ": commit mode writes into the repository that owns the organization, and this one has none — use mode apply"}
	case errors.Is(err, layout.ErrNoOrganization):
		return &ErrRefused{Reason: err.Error() + ": the repository that reconciles the Organization keeps no directory for it — add the organization's directory there, or use mode apply"}
	case errors.As(err, &secret):
		return &ErrRefused{Reason: fmt.Sprintf("%s: %s — cluster-manager commits a secret only encrypted for the repository's age recipients", strings.Join(secret.Paths, ", "), secret.Reason)}
	}
	return commitError(err)
}

// commitDeleteCluster opens delete_cluster's removal pull request in the
// repository that owns the organization: the cluster's directory, its Flux
// Kustomization and its entry (gitops-commit's RemoveCluster). The kserve
// backend registration cluster-manager wrote for the cluster is
// model-manager's runtime state, no file of the repository: it is removed
// live, as in apply. A cluster that is not in the repository was landed in
// apply mode and is removed in apply mode. The guard is apply's: a cluster
// whose release cluster-manager did not create, or a Cluster without that
// release, was made by other means and is removed by them.
func (s *Service) commitDeleteCluster(ctx context.Context, dyn dynamic.Interface, cluster, hr *unstructured.Unstructured, in DeleteClusterInput, out *WriteResult, start time.Time) error {
	ns := out.Namespace
	switch {
	case hr != nil && !compose.OwnedBy(hr):
		return &ErrRefused{Reason: fmt.Sprintf("HelmRelease %s/%s %s: delete_cluster removes only a cluster create_cluster created — delete it the way it was made", ns, in.Name, ownerDescription(hr))}
	case hr == nil && cluster != nil:
		return &ErrRefused{Reason: fmt.Sprintf("cluster %s/%s has no HelmRelease %s/%s: it was not created by create_cluster — delete it the way it was made", ns, in.Name, ns, in.Name)}
	}
	place, err := clusterPlace(ctx, dyn, s.cfg.Installation, in.Organization, in.Name)
	if err != nil {
		return err
	}
	remote, gh, err := s.gitHubFor(ctx)
	if err != nil {
		return err
	}
	built, err := layout.RemoveCluster(ctx, remote, place)
	if err != nil {
		return clusterLayoutError(err)
	}
	if !built.Changed() {
		return &ErrRefused{Reason: fmt.Sprintf("cluster %s is not in %s (no %s on %s): it was landed in mode apply — remove it with mode apply", in.Name, place.Repository, place.KustomizationPath(), place.Branch)}
	}
	plan := &commitPlan{Plan: built.Plan, target: &CommitTarget{
		Repository: place.Repository.String(), Branch: place.Branch, Path: place.Directory().Path(),
		Kustomization: place.Owner.Namespace + "/" + place.Owner.Name, Prune: built.Prune,
	}, remote: remote}
	registered, err := backendsOf(ctx, dyn, s.cfg.ModelManagerNamespace, in.Name)
	if err != nil {
		return err
	}
	if len(registered) > 0 {
		plans, err := planDeletes(ctx, dyn, backendRefs(s.cfg.ModelManagerNamespace, registered))
		if err != nil {
			return err
		}
		td := newTeardown(ctx, dyn, in.DryRun, out, s.budget(ctx, start, 0))
		if err := td.deleteAll(plans); err != nil {
			return err
		}
		td.finish()
	}
	if out.WithCluster, err = clusterReleases(ctx, dyn, ns, in.Name); err != nil {
		return err
	}
	title := fmt.Sprintf("feat(%s): remove workload cluster %s", in.Organization, in.Name)
	what := fmt.Sprintf("Removes the workload cluster `%s` from the organization `%s`: its Flux Kustomization `%s` and every file it applies", in.Name, in.Organization, built.Kustomization)
	res, err := plan.open(ctx, gh, "cluster-manager/remove-cluster-"+in.Name, title, commitBody(what, "delete_cluster", gh.Login, plan, nil), in.DryRun)
	if err != nil {
		return err
	}
	if built.Prune {
		res.LiveSteps = append(res.LiveSteps, fmt.Sprintf("Flux prunes Kustomization %s: the merge removes the cluster's Kustomization %s, which removes the cluster's release, and its uninstall the cluster; then delete_cluster {organization: %s, name: %s, mode: apply} removes what the uninstall left", plan.target.Kustomization, built.Kustomization, in.Organization, in.Name))
	} else {
		res.LiveSteps = append(res.LiveSteps, fmt.Sprintf("Flux does not prune Kustomization %s (spec.prune false): after the merge the cluster's Kustomization %s stays on the installation — delete_cluster {organization: %s, name: %s, mode: apply} deletes it, Flux then removes the cluster's release and its uninstall the cluster; the same call once more removes what the uninstall left", plan.target.Kustomization, built.Kustomization, in.Organization, in.Name))
	}
	out.Commit = res
	out.NextStep = commitNextStep(plan, res, in.DryRun, "the removal", gh.Login, "then the live step commit.liveSteps names")
	return nil
}
