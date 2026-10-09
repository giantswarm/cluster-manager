package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// deleteLab is cluster-delete.yaml's installation; dev01's kubeconfig names
// wc1's apiserver, whose fixture serves the model of wc1-serving.yaml.
func deleteLab(t *testing.T) (*lab, *Service) {
	t.Helper()
	l := newLab(t, "cluster-delete.yaml").target(t, wc1APIServer, "wc1-serving.yaml")
	return l, l.service(Config{Installation: "gazelle"})
}

func dev01Delete() DeleteClusterInput {
	return DeleteClusterInput{Organization: "acme", Name: "dev01", Mode: ModeApply}
}

// deleteLog records the deletes the installation is asked for, in order.
type deleteLog struct {
	mu      sync.Mutex
	deletes []string
}

func recordDeletes(t *testing.T, l *lab) *deleteLog {
	t.Helper()
	log := &deleteLog{}
	fakeInstallation(t, l).PrependReactor("delete", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		del, _ := a.(k8stesting.DeleteAction)
		log.mu.Lock()
		log.deletes = append(log.deletes, del.GetResource().Resource+" "+del.GetNamespace()+"/"+del.GetName())
		log.mu.Unlock()
		return false, nil, nil
	})
	return log
}

func (d *deleteLog) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.deletes...)
}

// TestDeleteClusterDryRun (golden): the backend registration, the release,
// its source and values would go; the pool, operator, slice and default-app
// releases go with the cluster and are listed, with the models served on it.
// Nothing is written.
func TestDeleteClusterDryRun(t *testing.T) {
	l, svc := deleteLab(t)
	log := recordDeletes(t, l)
	writes := recordWrites(t, l)
	in := dev01Delete()
	in.DryRun = true
	got, err := svc.DeleteCluster(context.Background(), in)
	require.NoError(t, err)
	assertGolden(t, "delete_cluster_dry_run", got)
	assert.Equal(t, []string{"org-acme/dev01-agent-platform", "org-acme/dev01-cert-exporter", "org-acme/dev01-gpu", "org-acme/dev01-gpu-operator"}, got.WithCluster)
	assert.NotEmpty(t, got.Models, "the models served on the cluster are listed")
	assert.Empty(t, log.seen())
	assert.Empty(t, writes.seen())
}

// TestDeleteClusterAppliesThenRemovesLeftovers: the first call removes the
// backend registration, then the release, its source and its values, as the
// caller; while the uninstall runs the re-run writes nothing; once the
// Cluster is gone it removes what the uninstall left — the default app's
// release and source — and names another owner's object without touching it.
// The third re-run finds nothing of the cluster.
func TestDeleteClusterAppliesThenRemovesLeftovers(t *testing.T) {
	l, svc := deleteLab(t)
	log := recordDeletes(t, l)
	ctx := context.Background()
	got, err := svc.DeleteCluster(ctx, dev01Delete())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"configmaps agent-platform/model-backend-kserve-dev01",
		"helmreleases org-acme/dev01",
		"ocirepositories org-acme/dev01",
		"configmaps org-acme/dev01-values",
	}, log.seen())
	assert.Contains(t, got.NextStep, "re-run delete_cluster with the same arguments afterwards")

	// helm-controller's uninstall: the Cluster is being removed, the
	// releases it owns go with it, the default app's install was still
	// running and its release stays.
	clusters := l.installation.Resource(ClusterGVR).Namespace("org-acme")
	c, err := clusters.Get(ctx, "dev01", metav1.GetOptions{})
	require.NoError(t, err)
	now := metav1.NewTime(time.Date(2026, 10, 2, 17, 0, 0, 0, time.UTC))
	c.SetDeletionTimestamp(&now)
	_, err = clusters.Update(ctx, c, metav1.UpdateOptions{})
	require.NoError(t, err)
	releases := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
	for _, owned := range []string{"dev01-gpu", "dev01-gpu-operator", "dev01-agent-platform"} {
		require.NoError(t, releases.Delete(ctx, owned, metav1.DeleteOptions{}))
	}
	byHand := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "dev01-extra", "namespace": "org-acme", "labels": map[string]any{compose.LabelCluster: "dev01"}},
	}}
	_, err = releases.Create(ctx, byHand, metav1.CreateOptions{})
	require.NoError(t, err)
	log.mu.Lock()
	log.deletes = nil
	log.mu.Unlock()

	uninstalling, err := svc.DeleteCluster(ctx, dev01Delete())
	require.NoError(t, err)
	assert.Empty(t, uninstalling.Objects)
	assert.Contains(t, uninstalling.NextStep, "is being removed since 2026-10-02T17:00:00Z")
	assert.Empty(t, log.seen())

	require.NoError(t, clusters.Delete(ctx, "dev01", metav1.DeleteOptions{}))
	log.mu.Lock()
	log.deletes = nil
	log.mu.Unlock()
	left, err := svc.DeleteCluster(ctx, dev01Delete())
	require.NoError(t, err)
	assert.Equal(t, []string{"helmreleases org-acme/dev01-cert-exporter", "ocirepositories org-acme/dev01-cert-exporter"}, log.seen())
	assert.Equal(t, []string{"delete", "delete"}, actions(left))
	require.Len(t, left.Warnings, 1)
	assert.Contains(t, left.Warnings[0], "HelmRelease org-acme/dev01-extra was not created by cluster-manager and is left")

	require.NoError(t, releases.Delete(ctx, "dev01-extra", metav1.DeleteOptions{}))
	_, err = svc.DeleteCluster(ctx, dev01Delete())
	var notFound *ErrNotFound
	require.True(t, errors.As(err, &notFound), "nothing of the cluster is left: %v", err)
	assert.Equal(t, &NotFound{Cluster: "dev01", Namespace: "org-acme", NothingLeft: true}, notFound.NotFound, "a caller reads the complete removal without parsing prose")
}

// TestDeleteClusterRemovesOrphanSliceChildren: a slice child whose uninstall
// failed while the cluster stood carries Flux's labels of the slice release
// only, no cluster label, and stays with helm-controller's finalizer once the
// cluster is gone. The second pass finds it through the cluster's kubeconfig
// Secret, suspends it — helm-controller then skips the uninstall and takes
// its finalizer off — and deletes it; a dry run names it and writes nothing.
// A release of another cluster, or one installing into the installation, is
// not touched.
func TestDeleteClusterRemovesOrphanSliceChildren(t *testing.T) {
	l, svc := deleteLab(t)
	ctx := context.Background()
	for _, gone := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{{HelmReleaseGVR, "dev01"}, {ClusterGVR, "dev01"}, {HelmReleaseGVR, "dev01-cert-exporter"}, {compose.OCIRepositoryGVR, "dev01-cert-exporter"}, {HelmReleaseGVR, "dev01-gpu"}, {HelmReleaseGVR, "dev01-gpu-operator"}, {HelmReleaseGVR, "dev01-agent-platform"}} {
		require.NoError(t, l.installation.Resource(gone.gvr).Namespace("org-acme").Delete(ctx, gone.name, metav1.DeleteOptions{}))
	}
	releases := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
	child := func(name, secret string) *unstructured.Unstructured {
		hr := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
			"metadata": map[string]any{
				"name": name, "namespace": "org-acme", "finalizers": []any{fluxFinalizer},
				"labels": map[string]any{compose.LabelManagedBy: helmManagedBy, labelFluxName: "dev01-agent-platform", labelFluxNamespace: "org-acme"},
			},
			"spec": map[string]any{"releaseName": "kserve-runtime-configs"},
		}}
		if secret != "" {
			require.NoError(t, unstructured.SetNestedField(hr.Object, secret, "spec", "kubeConfig", "secretRef", "name"))
		}
		return hr
	}
	for _, hr := range []*unstructured.Unstructured{
		child("dev01-kserve-runtime-configs", "dev01-kubeconfig"),
		child("dev01-kserve-llmisvc-resources", "dev01-kubeconfig"),
		child("dev02-kserve-runtime-configs", "dev02-kubeconfig"),
		child("dev01-local", ""),
	} {
		_, err := releases.Create(ctx, hr, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	var suspended []string
	fakeInstallation(t, l).PrependReactor("patch", "helmreleases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p, _ := a.(k8stesting.PatchAction)
		if string(p.GetPatch()) != "" && assert.Contains(t, string(p.GetPatch()), `"suspend":true`) {
			suspended = append(suspended, p.GetName())
		}
		return false, nil, nil
	})
	log := recordDeletes(t, l)

	in := dev01Delete()
	in.DryRun = true
	dry, err := svc.DeleteCluster(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete", "would-delete"}, actions(dry))
	require.Len(t, dry.Warnings, 1)
	assert.Contains(t, dry.Warnings[0], "2 HelmRelease(s) install into the cluster through dev01-kubeconfig, which is gone: they would be suspended and deleted")
	assert.Empty(t, log.seen())
	assert.Empty(t, suspended)

	got, err := svc.DeleteCluster(ctx, dev01Delete())
	require.NoError(t, err)
	assert.Equal(t, []string{"dev01-kserve-llmisvc-resources", "dev01-kserve-runtime-configs"}, suspended, "suspended before the delete")
	assert.Equal(t, []string{"helmreleases org-acme/dev01-kserve-llmisvc-resources", "helmreleases org-acme/dev01-kserve-runtime-configs"}, log.seen())
	assert.False(t, got.Partial)
	require.Len(t, got.Warnings, 1)
	assert.Contains(t, got.Warnings[0], "2 HelmRelease(s) installing into the gone cluster through dev01-kubeconfig were suspended and deleted, and helm-controller removed them without an uninstall")
	for _, kept := range []string{"dev02-kserve-runtime-configs", "dev01-local"} {
		_, err := releases.Get(ctx, kept, metav1.GetOptions{})
		require.NoError(t, err, "%s is not the gone cluster's", kept)
	}

	_, err = svc.DeleteCluster(ctx, dev01Delete())
	var notFound *ErrNotFound
	require.True(t, errors.As(err, &notFound), "nothing of the cluster is left: %v", err)
}

// TestDeleteClusterRefusals: every refusal names its reason and the way out,
// and comes before any write.
func TestDeleteClusterRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*DeleteClusterInput)
		want string
	}{
		"not cluster-manager's":              {func(in *DeleteClusterInput) { in.Name = "legacy" }, "HelmRelease org-acme/legacy is managed by Helm: delete_cluster removes only a cluster create_cluster created — delete it the way it was made"},
		"own cluster":                        {func(in *DeleteClusterInput) { in.Organization, in.Name = "giantswarm", "gazelle" }, "cluster gazelle is the installation's own cluster"},
		"in the inventory":                   {func(in *DeleteClusterInput) { in.Name = "gitops" }, "HelmRelease org-acme/gitops is in git: Flux Kustomization default/acme-clusters applies it"},
		"not reconciled":                     {func(in *DeleteClusterInput) { in.Name = "fresh" }, "HelmRelease org-acme/fresh carries no finalizers.fluxcd.io"},
		"no release":                         {func(in *DeleteClusterInput) { in.Name = "byhand" }, "cluster org-acme/byhand has no HelmRelease org-acme/byhand: it was not created by create_cluster"},
		"commit mode without the GitHub App": {func(in *DeleteClusterInput) { in.Mode = ModeCommit }, "mode commit (a pull request opened as you) is not offered by this server"},
		"bad name":                           {func(in *DeleteClusterInput) { in.Name = "a,b" }, `cluster name "a,b"`},
		"no organization":                    {func(in *DeleteClusterInput) { in.Organization = "" }, "organization: required"},
	} {
		t.Run(name, func(t *testing.T) {
			l, svc := deleteLab(t)
			log := recordDeletes(t, l)
			in := dev01Delete()
			tc.edit(&in)
			_, err := svc.DeleteCluster(context.Background(), in)
			assertRefused(t, err, tc.want)
			assert.Empty(t, log.seen())
		})
	}
}

// TestDeleteClusterInventoryDecides: a release labelled by a Kustomization
// that no longer lists it in its inventory — the removal was merged into a
// repository whose Kustomization does not prune — is deleted in mode apply.
func TestDeleteClusterInventoryDecides(t *testing.T) {
	l, svc := deleteLab(t)
	log := recordDeletes(t, l)
	in := dev01Delete()
	in.Name = "pruned"
	got, err := svc.DeleteCluster(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, []string{"delete"}, actions(got), "the release; it has no source or values in the fixture")
	assert.Equal(t, []string{"helmreleases org-acme/pruned"}, log.seen())
}

// TestDeleteClusterRefusesAGitOpsLeftover: a leftover a Kustomization applies
// refuses the second pass before any write.
func TestDeleteClusterRefusesAGitOpsLeftover(t *testing.T) {
	l, svc := deleteLab(t)
	ctx := context.Background()
	for _, gone := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{{HelmReleaseGVR, "dev01"}, {ClusterGVR, "dev01"}} {
		require.NoError(t, l.installation.Resource(gone.gvr).Namespace("org-acme").Delete(ctx, gone.name, metav1.DeleteOptions{}))
	}
	ks, err := l.installation.Resource(KustomizationGVR).Namespace("default").Get(ctx, "acme-clusters", metav1.GetOptions{})
	require.NoError(t, err)
	entries, _, _ := unstructured.NestedSlice(ks.Object, "status", "inventory", "entries")
	entries = append(entries, map[string]any{"id": "org-acme_dev01-cert-exporter_helm.toolkit.fluxcd.io_HelmRelease", "v": "v2"})
	require.NoError(t, unstructured.SetNestedSlice(ks.Object, entries, "status", "inventory", "entries"))
	_, err = l.installation.Resource(KustomizationGVR).Namespace("default").Update(ctx, ks, metav1.UpdateOptions{})
	require.NoError(t, err)
	hr, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "dev01-cert-exporter", metav1.GetOptions{})
	require.NoError(t, err)
	labels := hr.GetLabels()
	labels[labelKustomizeName], labels[labelKustomizeNamespace] = "acme-clusters", "default"
	hr.SetLabels(labels)
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)

	log := recordDeletes(t, l)
	_, err = svc.DeleteCluster(ctx, dev01Delete())
	assertRefused(t, err, "HelmRelease org-acme/dev01-cert-exporter is in git: Flux Kustomization default/acme-clusters applies it")
	assert.Empty(t, log.seen())
}
