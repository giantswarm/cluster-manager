package tools

import (
	"context"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
