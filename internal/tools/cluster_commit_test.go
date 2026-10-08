package tools

import (
	"context"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	acmeOrgDir   = "management-clusters/gazelle/organizations/acme"
	acmeClusters = acmeOrgDir + "/workload-clusters"
	dev01Dir     = acmeClusters + "/dev01"
)

// clusterCommitLab is the releases lab with the Organizations and the fleet
// repository as a Fake carrying base.
func clusterCommitLab(t *testing.T, base map[string][]byte) (*lab, *Service, *commit.Fake) {
	t.Helper()
	l := newLab(t, "releases.yaml")
	l.add(t, l.installation, "organizations.yaml")
	fake := commit.NewFake()
	fake.AddBranch(fleet, "main", base)
	return l, l.service(Config{Installation: "gazelle"}, WithChartReader(releaseCharts(t)), withFake(fake)), fake
}

// acmeBase is a gitops-template root with acme's directory and one cluster
// listed in its workload-clusters/kustomization.yaml.
func acmeBase() map[string][]byte {
	return map[string][]byte{
		acmeOrgDir + "/kustomization.yaml":   []byte("resources:\n  - workload-clusters\n"),
		acmeClusters + "/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - wc1.yaml\n"),
		acmeClusters + "/wc1.yaml":           []byte("kind: Kustomization\n"),
	}
}

func dev01Commit() CreateClusterInput {
	in := dev01()
	in.Mode = ModeCommit
	return in
}

// TestCreateClusterCommitDryRun (golden): the objects apply would land as
// files of the cluster's own directory, its Flux Kustomization (the
// organization's source, service account, interval, timeout and keys,
// pruning) and its entry beside wc1's; the repository, branch and directory
// named, nothing written anywhere.
func TestCreateClusterCommitDryRun(t *testing.T) {
	l, svc, fake := clusterCommitLab(t, acmeBase())
	log := recordWrites(t, l)
	in := dev01Commit()
	in.DryRun = true
	got, err := svc.CreateCluster(asPerson(context.Background()), in)
	require.NoError(t, err)
	assertGolden(t, "create_cluster_commit_dry_run", got)
	assert.Empty(t, log.seen())
	assert.Empty(t, fake.PullRequests())
}

// TestCreateClusterCommit: one pull request as the person whose files are
// the dry run's; a re-run once it is merged changes nothing. A node pool of
// the cluster then follows its Kustomization to <cluster>/cluster-manager/.
func TestCreateClusterCommit(t *testing.T) {
	l, svc, fake := clusterCommitLab(t, acmeBase())
	log := recordWrites(t, l)
	ctx := asPerson(context.Background())
	dry := dev01Commit()
	dry.DryRun = true
	planned, err := svc.CreateCluster(ctx, dry)
	require.NoError(t, err)

	got, err := svc.CreateCluster(ctx, dev01Commit())
	require.NoError(t, err)
	assert.Empty(t, log.seen(), "commit mode writes nothing on the installation")
	require.NotNil(t, got.Commit)
	prs := fake.PullRequests()
	require.Len(t, prs, 1)
	pr := prs[0]
	assert.Equal(t, got.Commit.PullRequest, pr.URL)
	assert.Equal(t, "jane", got.Commit.Author)
	assert.Equal(t, "cluster-manager/create-cluster-dev01", pr.Head)
	assert.Equal(t, "feat(acme): add workload cluster dev01", pr.Title)
	assert.Contains(t, pr.Body, "Opened by @jane through cluster-manager (`create_cluster`, mode commit)")
	assert.Contains(t, pr.Body, "`default/gazelle-clusters-dev01`")
	assert.Contains(t, got.NextStep, "review and merge "+pr.URL)

	head := fake.Files(fleet, pr.Head)
	for _, f := range planned.Commit.Files {
		assert.Equal(t, f.Content, string(head[f.Path]), "the pull request carries the dry run's %s", f.Path)
	}
	assert.Contains(t, string(head[acmeClusters+"/dev01.yaml"]), "path: ./"+dev01Dir+"\n")

	// Merged: the same call has nothing to commit.
	fake.AddBranch(fleet, "main", head)
	again, err := svc.CreateCluster(ctx, dev01Commit())
	require.NoError(t, err)
	assert.Empty(t, again.Commit.PullRequest)
	assert.Contains(t, again.NextStep, "already carries the cluster: nothing to commit")
	assert.Len(t, fake.PullRequests(), 1)
}

// TestCreateClusterCommitRefusals: each refusal names its reason and the way
// out, before anything is written or opened.
func TestCreateClusterCommitRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		base map[string][]byte
		edit func(*CreateClusterInput)
		ctx  func(context.Context) context.Context
		want string
	}{
		"organization not from git": {acmeBase(), func(in *CreateClusterInput) { in.Organization = "plain" }, asPerson,
			"organization plain: not reconciled by a Flux Kustomization: commit mode writes into the repository that owns the organization, and this one has none — use mode apply"},
		"no organization directory": {map[string][]byte{"README.md": []byte("fleet\n")}, func(*CreateClusterInput) {}, asPerson,
			"acme/workload-clusters-fleet@main has no " + acmeOrgDir + "/: the repository that reconciles the Organization keeps no directory for it"},
		"no GitHub authorization": {acmeBase(), func(*CreateClusterInput) {}, func(ctx context.Context) context.Context { return ctx },
			"core_auth_login server=cluster-manager"},
	} {
		t.Run(name, func(t *testing.T) {
			l, svc, fake := clusterCommitLab(t, tc.base)
			log := recordWrites(t, l)
			in := dev01Commit()
			tc.edit(&in)
			_, err := svc.CreateCluster(tc.ctx(context.Background()), in)
			assertRefused(t, err, tc.want)
			assert.Empty(t, log.seen())
			assert.Empty(t, fake.PullRequests())
		})
	}
}

// deleteCommitLab is delete_cluster's installation with the Organizations,
// and the fleet repository as a Fake carrying dev01 as create_cluster's
// commit wrote it (with a GPU pool under its cluster-manager directory)
// beside wc1.
func deleteCommitLab(t *testing.T, base map[string][]byte) (*lab, *Service, *commit.Fake) {
	t.Helper()
	l := newLab(t, "cluster-delete.yaml").target(t, wc1APIServer, "wc1-serving.yaml")
	l.add(t, l.installation, "organizations.yaml")
	fake := commit.NewFake()
	fake.AddBranch(fleet, "main", base)
	return l, l.service(Config{Installation: "gazelle"}, withFake(fake)), fake
}

func dev01InGit() map[string][]byte {
	base := acmeBase()
	base[acmeClusters+"/kustomization.yaml"] = []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - wc1.yaml\n  - dev01.yaml\n")
	base[acmeClusters+"/dev01.yaml"] = []byte("kind: Kustomization\n")
	base[dev01Dir+"/kustomization.yaml"] = []byte("resources:\n  - dev01.yaml\n  - dev01-values.yaml\n  - cluster-manager\n")
	base[dev01Dir+"/dev01.yaml"] = []byte("kind: HelmRelease\n")
	base[dev01Dir+"/dev01-values.yaml"] = []byte("kind: ConfigMap\n")
	base[dev01Dir+"/cluster-manager/kustomization.yaml"] = []byte("resources:\n  - dev01-gpu.yaml\n")
	base[dev01Dir+"/cluster-manager/dev01-gpu.yaml"] = []byte("kind: HelmRelease\n")
	return base
}

func dev01DeleteCommit() DeleteClusterInput {
	in := dev01Delete()
	in.Mode = ModeCommit
	return in
}

// TestDeleteClusterCommitDryRun (golden): the removal of the cluster's
// directory with its pools, its Kustomization and its entry; liveSteps names
// delete_cluster in mode apply after the merge (the organization's
// Kustomization does not prune). Nothing is written or opened.
func TestDeleteClusterCommitDryRun(t *testing.T) {
	l, svc, fake := deleteCommitLab(t, dev01InGit())
	log := recordDeletes(t, l)
	in := dev01DeleteCommit()
	in.DryRun = true
	got, err := svc.DeleteCluster(asPerson(context.Background()), in)
	require.NoError(t, err)
	assertGolden(t, "delete_cluster_commit_dry_run", got)
	assert.Empty(t, log.seen())
	assert.Empty(t, fake.PullRequests())
}

// TestDeleteClusterCommit: one removal pull request as the person, wc1 left
// listed; the backend registration goes live.
func TestDeleteClusterCommit(t *testing.T) {
	l, svc, fake := deleteCommitLab(t, dev01InGit())
	log := recordDeletes(t, l)
	got, err := svc.DeleteCluster(asPerson(context.Background()), dev01DeleteCommit())
	require.NoError(t, err)
	assert.Equal(t, []string{"configmaps agent-platform/model-backend-kserve-dev01"}, log.seen())
	prs := fake.PullRequests()
	require.Len(t, prs, 1)
	assert.Equal(t, "cluster-manager/remove-cluster-dev01", prs[0].Head)
	assert.Equal(t, "feat(acme): remove workload cluster dev01", prs[0].Title)
	assert.Equal(t, got.Commit.PullRequest, prs[0].URL)
	head := fake.Files(fleet, prs[0].Head)
	for p := range head {
		assert.NotContains(t, p, "dev01", "the removal leaves nothing of dev01")
	}
	assert.Equal(t, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - wc1.yaml\n", string(head[acmeClusters+"/kustomization.yaml"]))
	require.Len(t, got.Commit.LiveSteps, 1)
	assert.Contains(t, got.Commit.LiveSteps[0], "delete_cluster {organization: acme, name: dev01, mode: apply} deletes it")
}

// TestDeleteClusterCommitPruningOwner: an organization Kustomization that
// prunes removes the cluster with the merge; liveSteps says so.
func TestDeleteClusterCommitPruningOwner(t *testing.T) {
	l, svc, _ := deleteCommitLab(t, dev01InGit())
	setPrune(t, l, "gazelle-gitops", true)
	in := dev01DeleteCommit()
	in.DryRun = true
	got, err := svc.DeleteCluster(asPerson(context.Background()), in)
	require.NoError(t, err)
	require.Len(t, got.Commit.LiveSteps, 1)
	assert.Contains(t, got.Commit.LiveSteps[0], "Flux prunes Kustomization default/gazelle-gitops")
}

// TestDeleteClusterCommitRefusals: a cluster the repository does not carry
// was applied and is removed in mode apply; a release cluster-manager did
// not create, or a Cluster without one, is refused as in apply.
func TestDeleteClusterCommitRefusals(t *testing.T) {
	_, svc, fake := deleteCommitLab(t, acmeBase())
	_, err := svc.DeleteCluster(asPerson(context.Background()), dev01DeleteCommit())
	assertRefused(t, err, "cluster dev01 is not in acme/workload-clusters-fleet (no "+acmeClusters+"/dev01.yaml on main): it was landed in mode apply — remove it with mode apply")

	in := dev01DeleteCommit()
	in.Name = "legacy"
	_, err = svc.DeleteCluster(asPerson(context.Background()), in)
	assertRefused(t, err, "delete_cluster removes only a cluster create_cluster created")

	// byhand is in the repository, but its Cluster has no release of
	// create_cluster's: the removal is not cluster-manager's to open.
	inGitByHand := acmeBase()
	inGitByHand[acmeClusters+"/byhand.yaml"] = []byte("kind: Kustomization\n")
	_, svcByHand, fakeByHand := deleteCommitLab(t, inGitByHand)
	in.Name = "byhand"
	_, err = svcByHand.DeleteCluster(asPerson(context.Background()), in)
	assertRefused(t, err, "cluster org-acme/byhand has no HelmRelease org-acme/byhand: it was not created by create_cluster")
	assert.Empty(t, fakeByHand.PullRequests())
	assert.Empty(t, fake.PullRequests())
}

// TestDeleteClusterAppliesAMergedRemoval: after the removal merged into a
// repository that does not prune, dev01's own Kustomization is out of the
// organization's inventory while its release is still in that
// Kustomization's: mode apply deletes the backend registration and the
// Kustomization, whose prune removes the release. While the organization
// still lists the Kustomization, the release is in git and refused.
func TestDeleteClusterAppliesAMergedRemoval(t *testing.T) {
	l, svc, _ := deleteCommitLab(t, nil)
	ctx := context.Background()
	res := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
	hr, err := res.Get(ctx, "dev01", metav1.GetOptions{})
	require.NoError(t, err)
	labels := hr.GetLabels()
	labels[labelKustomizeName], labels[labelKustomizeNamespace] = "gazelle-clusters-dev01", "default"
	hr.SetLabels(labels)
	_, err = res.Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "gazelle-clusters-dev01", "namespace": "default", "labels": map[string]any{labelKustomizeName: "gazelle-gitops", labelKustomizeNamespace: "default"}},
		"spec":     map[string]any{"path": "./" + dev01Dir, "prune": true},
		"status":   map[string]any{"inventory": map[string]any{"entries": []any{map[string]any{"id": "org-acme_dev01_helm.toolkit.fluxcd.io_HelmRelease", "v": "v2"}}}},
	}}
	_, err = l.installation.Resource(KustomizationGVR).Namespace("default").Create(ctx, ks, metav1.CreateOptions{})
	require.NoError(t, err)

	setInventory(t, l, "gazelle-gitops", "default_gazelle-clusters-dev01_kustomize.toolkit.fluxcd.io_Kustomization")
	_, err = svc.DeleteCluster(ctx, dev01Delete())
	assertRefused(t, err, "HelmRelease org-acme/dev01 is in git: Flux Kustomization default/gazelle-clusters-dev01 applies it")

	setInventory(t, l, "gazelle-gitops")
	log := recordDeletes(t, l)
	got, err := svc.DeleteCluster(ctx, dev01Delete())
	require.NoError(t, err)
	assert.Equal(t, []string{"configmaps agent-platform/model-backend-kserve-dev01", "kustomizations default/gazelle-clusters-dev01"}, log.seen())
	assert.Contains(t, got.WithCluster, "org-acme/dev01-gpu")
}

func setPrune(t *testing.T, l *lab, name string, prune bool) {
	t.Helper()
	res := l.installation.Resource(KustomizationGVR).Namespace("default")
	ks, err := res.Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(ks.Object, prune, "spec", "prune"))
	_, err = res.Update(context.Background(), ks, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func setInventory(t *testing.T, l *lab, name string, ids ...string) {
	t.Helper()
	res := l.installation.Resource(KustomizationGVR).Namespace("default")
	ks, err := res.Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	entries := []any{}
	for _, id := range ids {
		entries = append(entries, map[string]any{"id": id, "v": "v1"})
	}
	require.NoError(t, unstructured.SetNestedSlice(ks.Object, entries, "status", "inventory", "entries"))
	_, err = res.Update(context.Background(), ks, metav1.UpdateOptions{})
	require.NoError(t, err)
}
