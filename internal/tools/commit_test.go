package tools

import (
	"context"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/identity"
)

var fleet = commit.Repository{Owner: "acme", Name: "workload-clusters-fleet"}

const (
	wc1Dir    = "management-clusters/gazelle/organizations/acme/workload-clusters/wc1"
	commitDir = wc1Dir + "/cluster-manager"
)

// commitLab is the lab with wc1 owned by the fleet's Kustomization and the
// fleet repository as a Fake carrying base.
func commitLab(t *testing.T, base map[string][]byte) (*lab, *commit.Fake) {
	t.Helper()
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "commit.yaml")
	res := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
	hr, err := res.Get(context.Background(), "wc1", metav1.GetOptions{})
	require.NoError(t, err)
	labels := hr.GetLabels()
	labels[labelKustomizeName], labels[labelKustomizeNamespace] = "acme-clusters-wc1", "default"
	hr.SetLabels(labels)
	_, err = res.Update(context.Background(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	fake := commit.NewFake()
	fake.AddBranch(fleet, "main", base)
	return l, fake
}

// sopsAge is a .sops.yaml encrypting the fleet's *.enc.yaml files for a
// fresh age recipient.
func sopsAge(t *testing.T) []byte {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	return []byte("creation_rules:\n- path_regex: .*\\.enc\\.yaml\n  encrypted_regex: ^(data|stringData)$\n  age: " + id.Recipient().String() + "\n")
}

func withFake(fake *commit.Fake) Option {
	return WithGitHub("cluster-manager-commit", func(string) (GitHubRemote, error) { return fake, nil })
}

func asPerson(ctx context.Context) context.Context {
	return identity.ContextWithGitHub(ctx, &identity.GitHub{Login: "jane", Token: "the-persons-token"})
}

func commitMode(in CreateNodePoolInput) CreateNodePoolInput {
	in.Mode = ModeCommit
	return in
}

// TestCreateNodePoolCommit: the pool's, the operator's and the slice's
// releases land as files of wc1's cluster-manager directory in one pull
// request as the person, the directory's kustomization.yaml lists them, the
// parent directory (no kustomization.yaml of its own: Flux generates one)
// is left alone, and the backend registration is written live.
func TestCreateNodePoolCommit(t *testing.T) {
	l, fake := commitLab(t, map[string][]byte{wc1Dir + "/cluster/kustomization.yaml": []byte("resources: []\n"), ".sops.yaml": sopsAge(t)})
	svc := l.service(Config{Installation: "gazelle"}, withFake(fake))
	in := commitMode(l4("wc1", "gpu-l4", false))
	in.Pool.Name = "gpu-l4"
	out, err := svc.CreateNodePool(asPerson(context.Background()), in)
	require.NoError(t, err)
	require.NotNil(t, out.Commit)
	assert.Equal(t, "acme/workload-clusters-fleet", out.Commit.Repository)
	assert.Equal(t, commitDir, out.Commit.Directory)
	assert.Equal(t, "default/acme-clusters-wc1", out.Commit.Kustomization)
	assert.Equal(t, "jane", out.Commit.Author)
	assert.NotEmpty(t, out.Commit.PullRequest)

	prs := fake.PullRequests()
	require.Len(t, prs, 1)
	assert.Equal(t, "feat(wc1): add GPU node pool gpu-l4", prs[0].Title)
	assert.Contains(t, prs[0].Body, "Opened by @jane through cluster-manager (`create_node_pool`, mode commit)")
	assert.Equal(t, "cluster-manager/add-wc1-gpu-l4", prs[0].Head)

	files := fake.Files(fleet, prs[0].Head)
	for _, name := range []string{"wc1-gpu-l4.yaml", "wc1-gpu-operator.yaml", "wc1-agent-platform.yaml", "kustomization.yaml"} {
		assert.Contains(t, files, commitDir+"/"+name)
	}
	assert.NotContains(t, files, wc1Dir+"/kustomization.yaml", "Flux generates the parent's kustomization: nothing to add")
	pool := string(files[commitDir+"/wc1-gpu-l4.yaml"])
	assert.Contains(t, pool, "kind: OCIRepository")
	assert.Contains(t, pool, "kind: HelmRelease")
	assert.NotContains(t, pool, "ownerReferences", "no live UID in git")
	resources, err := kustomizationResources(files[commitDir+"/kustomization.yaml"])
	require.NoError(t, err)
	assert.Equal(t, []string{"wc1-agent-platform.yaml", "wc1-gpu-l4-values-secret.enc.yaml", "wc1-gpu-l4.yaml", "wc1-gpu-operator.yaml"}, resources)
	secret := string(files[commitDir+"/wc1-gpu-l4-values-secret.enc.yaml"])
	assert.Contains(t, secret, "ENC[AES256_GCM", "the registry credentials are encrypted for the repository's age recipients")
	assert.NotContains(t, secret, "s3cret", "no credential in plaintext")

	var backend bool
	for _, o := range out.Objects {
		if o.Kind == "ConfigMap" && o.Name == compose.BackendConfigMapName {
			backend = o.Action == actionCreate
		}
		assert.NotEqual(t, "HelmRelease", o.Kind, "no release is applied live")
	}
	assert.True(t, backend, "the backend registration is written live")
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(context.Background(), "wc1-gpu-l4", metav1.GetOptions{})
	assert.Error(t, err, "the pool release lands through the merge, not live")
}

// TestCreateNodePoolCommitDryRunAndRerun: the dry run shows the files with
// their content (a secret file without) and opens nothing; a re-run against
// a base that carries the files already opens nothing either.
func TestCreateNodePoolCommitDryRunAndRerun(t *testing.T) {
	l, fake := commitLab(t, map[string][]byte{".sops.yaml": sopsAge(t)})
	svc := l.service(Config{Installation: "gazelle"}, withFake(fake))
	ctx := asPerson(context.Background())
	dry, err := svc.CreateNodePool(ctx, commitMode(l4("wc1", "gpu-l4", true)))
	require.NoError(t, err)
	assert.Empty(t, fake.PullRequests(), "a dry run opens nothing")
	assert.Empty(t, dry.Commit.PullRequest)
	assert.Contains(t, dry.NextStep, "re-run without dryRun to open the pull request as jane")
	for _, f := range dry.Commit.Files {
		assert.Equal(t, fileAdd, f.Action, f.Path)
		if strings.HasSuffix(f.Path, ".enc.yaml") {
			assert.Empty(t, f.Content, "a secret file's content is never shown")
		} else {
			assert.NotEmpty(t, f.Content, f.Path)
		}
	}

	out, err := svc.CreateNodePool(ctx, commitMode(l4("wc1", "gpu-l4", false)))
	require.NoError(t, err)
	merged := fake.Files(fleet, out.Commit.Branch)
	l2, fake2 := commitLab(t, merged)
	again, err := l2.service(Config{Installation: "gazelle"}, withFake(fake2)).CreateNodePool(ctx, commitMode(l4("wc1", "gpu-l4", false)))
	require.NoError(t, err)
	assert.Empty(t, fake2.PullRequests(), "nothing changed: no pull request")
	assert.Contains(t, again.NextStep, "already carries the pool")
	for _, f := range again.Commit.Files {
		assert.Equal(t, fileUnchanged, f.Action, f.Path)
	}
}

// TestCreateNodePoolCommitAddsTheParentEntry: a parent directory with its
// own kustomization.yaml gets the cluster-manager entry, its comments and
// other keys kept.
func TestCreateNodePoolCommitAddsTheParentEntry(t *testing.T) {
	parent := "# wc1's objects\napiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - cluster # the cluster itself\n  - apps\n"
	l, fake := commitLab(t, map[string][]byte{".sops.yaml": sopsAge(t), wc1Dir + "/kustomization.yaml": []byte(parent)})
	out, err := l.service(Config{Installation: "gazelle"}, withFake(fake)).CreateNodePool(asPerson(context.Background()), commitMode(l4("wc1", "gpu-l4", false)))
	require.NoError(t, err)
	got := string(fake.Files(fleet, out.Commit.Branch)[wc1Dir+"/kustomization.yaml"])
	assert.Contains(t, got, "# wc1's objects")
	assert.Contains(t, got, "- cluster # the cluster itself")
	assert.Contains(t, got, "- cluster-manager")
}

// TestCreateNodePoolCommitRefusals: commit mode needs the App-pinned
// registration, the person's GitHub authorization, a cluster Flux owns, and
// age recipients for the credentials — each refused before anything lands.
func TestCreateNodePoolCommitRefusals(t *testing.T) {
	l, fake := commitLab(t, map[string][]byte{})
	ctx := asPerson(context.Background())

	_, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, commitMode(l4("wc1", "gpu-l4", false)))
	assertRefused(t, err, "mode commit (a pull request opened as you) is not offered by this server: it is not registered with its GitHub App (chart value github.enabled), so it holds no GitHub authorization of yours — use mode apply")

	// The main registration forwards the IdP token alone: commit mode there
	// names the commit registration, for either tool, dry run or not.
	svc := l.service(Config{Installation: "gazelle"}, withFake(fake))
	const viaCommit = "mode commit opens the pull request with your GitHub authorization, which only the commit registration cluster-manager-commit carries, and this call came without it: connect cluster-manager-commit in muster (core_auth_login server=cluster-manager-commit, your consent to the server's GitHub App) and call the tool through it (x_cluster-manager-commit_<tool>)"
	dry := commitMode(l4("wc1", "gpu-l4", false))
	dry.DryRun = true
	_, err = svc.CreateNodePool(context.Background(), dry)
	assertRefused(t, err, viaCommit)
	_, err = svc.DeleteNodePool(context.Background(), DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-l4", Mode: ModeCommit})
	assertRefused(t, err, viaCommit)

	_, err = svc.CreateNodePool(ctx, commitMode(l4("wc1", "gpu-l4", false)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wc1-gpu-l4-values-secret.enc.yaml holds the pool's registry credentials, and acme/workload-clusters-fleet has no .sops.yaml")

	_, err = svc.CreateNodePool(ctx, commitMode(l4("wc2", "gpu-l4b", false)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Flux Kustomization owns cluster org-acme/wc2")
	assert.Contains(t, err.Error(), "use mode apply")

	pgp, fakePGP := commitLab(t, map[string][]byte{".sops.yaml": []byte("creation_rules:\n- path_regex: .*\\.enc\\.yaml\n  pgp: 262E46EC171FF16AC517D37BD17F605E180DDAF8\n")})
	_, err = pgp.service(Config{Installation: "gazelle"}, withFake(fakePGP)).CreateNodePool(ctx, commitMode(l4("wc1", "gpu-l4", false)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "its .sops.yaml cannot be used")
	assert.Empty(t, fake.PullRequests())
	assert.Empty(t, fakePGP.PullRequests())

	_, err = svc.EnableModelServing(ctx, ModelServingInput{Cluster: "wc1", Mode: ModeCommit})
	assertRefused(t, err, "mode commit covers create_node_pool and delete_node_pool; this tool lands its objects in mode apply only")
}

// TestDeleteNodePoolCommit: the removal pull request deletes the pool's
// files and its kustomization entry; the answer names the live step a
// Kustomization that does not prune leaves. A pool that is not in the
// repository is refused.
func TestDeleteNodePoolCommit(t *testing.T) {
	base := map[string][]byte{
		commitDir + "/wc1-gpu-a10g.yaml":  []byte("kind: HelmRelease\n"),
		commitDir + "/wc1-other.yaml":     []byte("kind: HelmRelease\n"),
		commitDir + "/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - wc1-gpu-a10g.yaml\n  - wc1-other.yaml\n"),
	}
	l, fake := commitLab(t, base)
	l.target(t, wc1APIServer, "wc1-nodeclaims.yaml", servingAPIs...)
	svc := l.service(Config{Installation: "gazelle"}, withFake(fake))
	ctx := asPerson(context.Background())
	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeCommit})
	require.NoError(t, err)
	prs := fake.PullRequests()
	require.Len(t, prs, 1)
	assert.Equal(t, "feat(wc1): remove GPU node pool gpu-a10g", prs[0].Title)
	files := fake.Files(fleet, prs[0].Head)
	assert.NotContains(t, files, commitDir+"/wc1-gpu-a10g.yaml")
	resources, err := kustomizationResources(files[commitDir+"/kustomization.yaml"])
	require.NoError(t, err)
	assert.Equal(t, []string{"wc1-other.yaml"}, resources)
	require.Len(t, out.Commit.LiveSteps, 1)
	assert.Contains(t, out.Commit.LiveSteps[0], "does not prune Kustomization default/acme-clusters-wc1")
	assert.Contains(t, out.Commit.LiveSteps[0], "delete_node_pool {cluster: wc1, name: gpu-a10g, mode: apply}")
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(context.Background(), "wc1-gpu-a10g", metav1.GetOptions{})
	assert.NoError(t, err, "the release stays until the merge")

	l2, fake2 := commitLab(t, map[string][]byte{})
	l2.target(t, wc1APIServer, "wc1-nodeclaims.yaml", servingAPIs...)
	_, err = l2.service(Config{Installation: "gazelle"}, withFake(fake2)).DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeCommit})
	assertRefused(t, err, "node pool gpu-a10g is not in acme/workload-clusters-fleet (no "+commitDir+"/wc1-gpu-a10g.yaml on main): it was landed in mode apply — remove it with mode apply")
}

// TestApplyRefusesWhatFluxAppliesFromGit: once the Kustomization's
// inventory lists the pool release, apply mode neither changes nor deletes
// it live — Flux would undo it.
func TestApplyRefusesWhatFluxAppliesFromGit(t *testing.T) {
	l, _ := commitLab(t, nil)
	l.target(t, wc1APIServer, "wc1-nodeclaims.yaml", servingAPIs...)
	ctx := context.Background()
	res := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
	hr, err := res.Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	require.NoError(t, err)
	labels := hr.GetLabels()
	labels[labelKustomizeName], labels[labelKustomizeNamespace] = "acme-clusters-wc1", "default"
	hr.SetLabels(labels)
	_, err = res.Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	svc := l.service(Config{Installation: "gazelle"})

	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	require.NoError(t, err, "not in the inventory: the live remainder of a removal from git")

	l, _ = commitLab(t, nil)
	l.target(t, wc1APIServer, "wc1-nodeclaims.yaml", servingAPIs...)
	res = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
	hr, err = res.Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	require.NoError(t, err)
	hr.SetLabels(labels)
	_, err = res.Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	ksRes := l.installation.Resource(KustomizationGVR).Namespace("default")
	ks, err := ksRes.Get(ctx, "acme-clusters-wc1", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(ks.Object, []any{map[string]any{"id": "org-acme_wc1-gpu-a10g_helm.toolkit.fluxcd.io_HelmRelease", "v": "v2"}}, "status", "inventory", "entries"))
	_, err = ksRes.Update(ctx, ks, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = l.service(Config{Installation: "gazelle"}).DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	assertRefused(t, err, "HelmRelease org-acme/wc1-gpu-a10g is in git: Flux Kustomization default/acme-clusters-wc1 applies it, so a live delete would be undone — remove the pool with mode commit, and after the merge, where the Kustomization does not prune, with mode apply")
}

// TestCommitTargetInListClusters: list_clusters names wc1's commit target
// from its Kustomization, and none for wc2, which no Kustomization owns.
func TestCommitTargetInListClusters(t *testing.T) {
	l, _ := commitLab(t, nil)
	list, err := l.service(Config{Installation: "gazelle"}).ListClusters(context.Background())
	require.NoError(t, err)
	byName := map[string]Cluster{}
	for _, c := range list.Clusters {
		byName[c.Name] = c
	}
	assert.Equal(t, &CommitTarget{Repository: "acme/workload-clusters-fleet", Branch: "main", Path: commitDir, Kustomization: "default/acme-clusters-wc1"}, byName["wc1"].CommitTarget)
	assert.Nil(t, byName["wc2"].CommitTarget)
}

// TestWithResourcesKeepsTheRest: the resources list is replaced, every
// other key and comment kept; nil content is a new kustomization.
func TestWithResourcesKeepsTheRest(t *testing.T) {
	out, err := withResources([]byte("# head\nkind: Kustomization\nnamespace: org-acme # ns\nresources: [a.yaml]\n"), []string{"a.yaml", "b.yaml"})
	require.NoError(t, err)
	assert.Equal(t, "# head\nkind: Kustomization\nnamespace: org-acme # ns\nresources: [a.yaml, b.yaml]\n", string(out))
	fresh, err := withResources(nil, []string{"x.yaml"})
	require.NoError(t, err)
	assert.Equal(t, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - x.yaml\n", string(fresh))
}
