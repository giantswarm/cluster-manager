package tools

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/identity"
)

// poolCommit is a node-pool write in commit mode: the releases go into the
// repository that owns the cluster as one pull request opened as the caller;
// the kserve backend registration — model-manager's runtime state on the
// installation, no object of the cluster's repository — is written live, as
// the caller, as in apply mode.
type poolCommit struct {
	loc    commitLocation
	remote GitHubRemote
	gh     *identity.GitHub
}

// newPoolCommit resolves the cluster's repository and the caller's GitHub
// authorization, both before anything is written.
func (s *Service) newPoolCommit(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (*poolCommit, error) {
	loc, err := commitLocationFor(ctx, dyn, c)
	if err != nil {
		return nil, err
	}
	remote, gh, err := s.gitHubFor(ctx)
	if err != nil {
		return nil, err
	}
	return &poolCommit{loc: loc, remote: remote, gh: gh}, nil
}

// fileOf is the file a release object lives in (releaseFiles' naming).
func (pc *poolCommit) fileOf(kind, name string) string {
	if kind == "Secret" {
		return path.Join(pc.loc.dir(), name+"-secret.enc.yaml")
	}
	return path.Join(pc.loc.dir(), name+".yaml")
}

// commitCreate lands create_node_pool's releases as the pull request: the
// pool's (and, where composed, the operator's and the slice's) files, the
// stale credentials file of a pool that no longer carries credentials
// removed, the backend document written live.
func (s *Service) commitCreate(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, in CreateNodePoolInput, releases, live []*unstructured.Unstructured, out *WriteResult, start time.Time) error {
	pc, err := s.newPoolCommit(ctx, dyn, c)
	if err != nil {
		return err
	}
	files, err := releaseFiles(pc.loc.dir(), releases)
	if err != nil {
		return err
	}
	var remove []string
	if stale := pc.fileOf("Secret", compose.ValuesSecretName(c.GetName(), in.Pool.Name)); files[stale] == nil {
		remove = append(remove, stale)
	}
	plan, err := planCommit(ctx, pc.remote, pc.loc, files, remove)
	if err != nil {
		return err
	}
	for _, obj := range releases {
		out.Manifests = append(out.Manifests, redacted(obj))
	}
	if err := applyAll(ctx, dyn, live, in.DryRun, out, s.budget(ctx, start)); err != nil {
		return err
	}
	title := fmt.Sprintf("feat(%s): add GPU node pool %s", c.GetName(), in.Pool.Name)
	body := pc.body(fmt.Sprintf("Adds the GPU node pool `%s` to the cluster `%s/%s`", in.Pool.Name, c.GetNamespace(), c.GetName()), "create_node_pool", plan, releases)
	res, err := plan.open(ctx, pc.gh, "cluster-manager/add-"+compose.ReleaseName(c.GetName(), in.Pool.Name), title, body, in.DryRun)
	if err != nil {
		return err
	}
	out.Commit = res
	out.NextStep = pc.nextStep(plan, res, in.DryRun, "the pool")
	return nil
}

// commitDelete opens delete_node_pool's removal pull request: the files of
// every release the live removal would delete (removalTargets), the backend
// document re-written or removed live. A pool that is not in the repository
// was landed in apply mode and is removed in apply mode.
func (s *Service) commitDelete(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, in DeleteNodePoolInput, targets []objectRef, backend *applyPlan, last bool, out *WriteResult, start time.Time) error {
	pc, err := s.newPoolCommit(ctx, dyn, c)
	if err != nil {
		return err
	}
	release := compose.ReleaseName(c.GetName(), in.Name)
	poolFile := pc.fileOf("HelmRelease", release)
	if raw, err := readBase(ctx, pc.remote, pc.loc, poolFile); err != nil {
		return err
	} else if raw == nil {
		return &ErrRefused{Reason: fmt.Sprintf("node pool %s is not in %s (no %s on %s): it was landed in mode apply — remove it with mode apply", in.Name, pc.loc.Repository, poolFile, pc.loc.Branch)}
	}
	var remove []string
	var liveTargets []objectRef
	for _, t := range targets {
		switch t.gvr {
		case HelmReleaseGVR, compose.OCIRepositoryGVR:
			remove = append(remove, pc.fileOf("HelmRelease", t.name))
		case compose.SecretGVR:
			remove = append(remove, pc.fileOf("Secret", t.name))
		default:
			liveTargets = append(liveTargets, t)
		}
	}
	plan, err := planCommit(ctx, pc.remote, pc.loc, nil, dedupe(remove))
	if err != nil {
		return err
	}
	if backend != nil {
		out.Backend = &BackendRegistration{Kind: compose.BackendKindKServe, Namespace: backend.obj.GetNamespace(), Name: backend.obj.GetName()}
		if err := applyAll(ctx, dyn, []*unstructured.Unstructured{backend.obj}, in.DryRun, out, s.budget(ctx, start)); err != nil {
			return err
		}
	}
	for _, t := range liveTargets {
		act, err := deleteIfOwned(ctx, dyn, t.gvr, t.ns, t.name, in.DryRun)
		if err != nil {
			return err
		}
		if act != nil {
			out.Objects = append(out.Objects, *act)
		}
	}
	what := fmt.Sprintf("Removes the GPU node pool `%s` from the cluster `%s/%s`", in.Name, c.GetNamespace(), c.GetName())
	if last {
		what += ", its last pool: the GPU operator and the serving layer cluster-manager composed go with it"
	}
	title := fmt.Sprintf("feat(%s): remove GPU node pool %s", c.GetName(), in.Name)
	res, err := plan.open(ctx, pc.gh, "cluster-manager/remove-"+release, title, pc.body(what, "delete_node_pool", plan, nil), in.DryRun)
	if err != nil {
		return err
	}
	if pc.loc.prune {
		res.LiveSteps = append(res.LiveSteps, fmt.Sprintf("Flux prunes Kustomization %s: the merge removes the releases from the installation as Flux deletes them, without delete_node_pool's ordered teardown of the serving layer", pc.loc.kustomization))
	} else {
		res.LiveSteps = append(res.LiveSteps, fmt.Sprintf("Flux does not prune Kustomization %s (spec.prune false): after the merge the releases stay on the installation — remove them with delete_node_pool {cluster: %s, name: %s, mode: apply}, which tears the serving layer down in order", pc.loc.kustomization, c.GetName(), in.Name))
	}
	out.Commit = res
	out.NextStep = pc.nextStep(plan, res, in.DryRun, "the removal")
	return nil
}

// body is the pull request's description: what it does, who asked through
// which tool, the files, and what lands them.
func (pc *poolCommit) body(what, tool string, plan *commitPlan, releases []*unstructured.Unstructured) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s.\n\nOpened by @%s through cluster-manager (`%s`, mode commit).\n\n", what, pc.gh.Login, tool)
	if len(releases) > 0 {
		b.WriteString("Releases:\n\n")
		for _, obj := range releases {
			if obj.GetKind() == "HelmRelease" {
				fmt.Fprintf(&b, "- `%s/%s` (chart %s %s)\n", obj.GetNamespace(), obj.GetName(), nestedString(obj, "spec", "chart", "spec", "chart"), chartVersionOf(releases, obj.GetName()))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("Files:\n\n")
	for _, f := range plan.actions {
		if f.Action != fileUnchanged {
			fmt.Fprintf(&b, "- %s `%s`\n", f.Action, f.Path)
		}
	}
	fmt.Fprintf(&b, "\nFlux Kustomization `%s` applies the change after the merge.\n", pc.loc.kustomization)
	return b.String()
}

// chartVersionOf is the tag of the OCIRepository of the release's name.
func chartVersionOf(objs []*unstructured.Unstructured, name string) string {
	for _, obj := range objs {
		if obj.GetKind() == "OCIRepository" && obj.GetName() == name {
			return nestedString(obj, "spec", "ref", "tag")
		}
	}
	return ""
}

// nextStep says what follows the commit.
func (pc *poolCommit) nextStep(plan *commitPlan, res *CommitResult, dryRun bool, what string) string {
	switch {
	case !plan.changed():
		return fmt.Sprintf("%s already carries %s: nothing to commit", res.Repository, what)
	case dryRun:
		return fmt.Sprintf("re-run without dryRun to open the pull request as %s in %s", pc.gh.Login, res.Repository)
	}
	return fmt.Sprintf("review and merge %s; Flux Kustomization %s applies it within its interval, and list_clusters shows the result", res.PullRequest, pc.loc.kustomization)
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	out := list[:0]
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// inGit names the Kustomization whose inventory still lists obj, "" when
// none does: an object Flux applies from git is changed or removed in git,
// never live. An object whose Kustomization no longer lists it — removed
// from git on a Kustomization that does not prune — is the live remainder a
// write in apply mode removes.
func inGit(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) (string, error) {
	labels := obj.GetLabels()
	name, ns := labels[labelKustomizeName], labels[labelKustomizeNamespace]
	if name == "" {
		return "", nil
	}
	ks, err := dyn.Resource(KustomizationGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get Kustomization %s/%s: %w", ns, name, err)
	}
	gv, _ := parseGroupVersion(obj.GetAPIVersion())
	id := strings.Join([]string{obj.GetNamespace(), obj.GetName(), gv, obj.GetKind()}, "_")
	entries, _, _ := unstructured.NestedSlice(ks.Object, "status", "inventory", "entries")
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && m["id"] == id {
			return ns + "/" + name, nil
		}
	}
	return "", nil
}

// parseGroupVersion is the API group of an apiVersion ("" for the core group).
func parseGroupVersion(apiVersion string) (string, string) {
	group, version, ok := strings.Cut(apiVersion, "/")
	if !ok {
		return "", apiVersion
	}
	return group, version
}
