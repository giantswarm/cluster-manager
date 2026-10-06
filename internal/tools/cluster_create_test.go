package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// releaseCharts is the registry as the release tests need it: release-aws
// 35.1.1 and 36.0.0 published with the real 36.0.0 chart's values.yaml and
// values.schema.json (compose's fixture, extracted unchanged from the
// archive), release-eks 34.0.1 published, and no release-azure tag.
func releaseCharts(t *testing.T) *fakeCharts {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"values.yaml", "values.schema.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "compose", "testdata", "charts", "release-aws-36.0.0", name)) //nolint:gosec // fixture named by the test
		require.NoError(t, err)
		files[name] = string(raw)
	}
	return (&fakeCharts{}).
		add(compose.ReleaseChartURL("aws"), "35.1.1", files).
		add(compose.ReleaseChartURL("aws"), "36.0.0", files).
		add(compose.ReleaseChartURL("eks"), "34.0.1", map[string]string{}).
		add(compose.ReleaseChartURL("azure"), "35.0.0", map[string]string{})
}

func releasesService(t *testing.T) (*lab, *Service) {
	t.Helper()
	l := newLab(t, "releases.yaml")
	return l, l.service(Config{Installation: "gazelle"}, WithChartReader(releaseCharts(t)))
}

// dev01 is the smallest create: an AWS cluster in acme on the newest active
// release, under the acme-dev identity.
func dev01() CreateClusterInput {
	return CreateClusterInput{Organization: "acme", Name: "dev01", Provider: "aws", Identity: "acme-dev", Description: "Acme's development cluster", Mode: ModeApply}
}

// TestListReleases (golden): newest first per line, each with its release
// chart and whether the registry lists its tag; offered only for an active
// release of an offered line whose tag is published, note saying why not.
func TestListReleases(t *testing.T) {
	_, svc := releasesService(t)
	got, err := svc.ListReleases(context.Background(), "")
	require.NoError(t, err)
	assertGolden(t, "list_releases", got)

	aws, err := svc.ListReleases(context.Background(), "aws")
	require.NoError(t, err)
	var names []string
	for _, r := range aws.Releases {
		names = append(names, r.Name)
	}
	assert.Equal(t, []string{"aws-37.0.0", "aws-36.0.0", "aws-35.1.1", "aws-34.0.0"}, names)
}

// TestListReleasesWithoutRegistry: a registry that cannot be read leaves
// published unknown with the reason, and nothing offered — never a guess.
func TestListReleasesWithoutRegistry(t *testing.T) {
	l := newLab(t, "releases.yaml")
	got, err := l.service(Config{Installation: "gazelle"}).ListReleases(context.Background(), "aws")
	require.NoError(t, err)
	for _, r := range got.Releases {
		assert.Nil(t, r.ReleaseChart.Published, r.Name)
		assert.Contains(t, r.ReleaseChart.Note, "reads no registry", r.Name)
		assert.False(t, r.Offered, r.Name)
	}
}

// TestCreateClusterDryRun (golden): the newest active release (36.0.0, not
// the preview 37.0.0), the three objects rendered, nothing written.
func TestCreateClusterDryRun(t *testing.T) {
	l, svc := releasesService(t)
	log := recordWrites(t, l)
	in := dev01()
	in.DryRun = true
	got, err := svc.CreateCluster(context.Background(), in)
	require.NoError(t, err)
	assertGolden(t, "create_cluster_dry_run", got)
	assert.Empty(t, log.seen())
}

// TestCreateClusterAppliesAndReruns: apply creates the source, the values and
// the release as the caller; once the cluster exists, the same call is a
// re-run that changes nothing.
func TestCreateClusterAppliesAndReruns(t *testing.T) {
	l, svc := releasesService(t)
	log := recordWrites(t, l)
	ctx := context.Background()
	got, err := svc.CreateCluster(ctx, dev01())
	require.NoError(t, err)
	assert.Equal(t, "aws-36.0.0", got.Release)
	assert.Equal(t, []string{"create OCIRepository dev01", "create ConfigMap dev01-values", "create HelmRelease dev01"}, log.seen())

	// The release chart renders the Cluster.
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ClusterGVR.GroupVersion().String(), "kind": "Cluster",
		"metadata": map[string]any{"name": "dev01", "namespace": "org-acme"},
	}}
	_, err = l.installation.Resource(ClusterGVR).Namespace("org-acme").Create(ctx, cluster, metav1.CreateOptions{})
	require.NoError(t, err)
	log.reset()

	again, err := svc.CreateCluster(ctx, dev01())
	require.NoError(t, err)
	for _, o := range again.Objects {
		assert.Equal(t, actionUnchanged, o.Action, o.String())
	}
	assert.Empty(t, log.seen())
}

// TestCreateClusterTrustsTheInstallationIdP: with the installation's Dex
// configured the cluster's values carry it in global.controlPlane.oidc and
// the answer names it; a caller's own block wins whole and is named as
// theirs; without one the answer says the cluster trusts no provider.
func TestCreateClusterTrustsTheInstallationIdP(t *testing.T) {
	oidc := &compose.ClusterOIDC{IssuerURL: "https://dex.gazelle.example", ClientID: "dex-k8s-authenticator", UsernameClaim: "email", GroupsClaim: "groups", CAPem: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"}
	l := newLab(t, "releases.yaml")
	svc := l.service(Config{Installation: "gazelle", ClusterOIDC: oidc}, WithChartReader(releaseCharts(t)))
	ctx := context.Background()
	in := dev01()
	in.DryRun = true
	got, err := svc.CreateCluster(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, &ClusterTrust{Source: TrustInstallation, IssuerURL: oidc.IssuerURL, ClientID: oidc.ClientID, UsernameClaim: "email", GroupsClaim: "groups", CA: true}, got.OIDC)
	assert.Contains(t, clusterValuesOf(t, got), "issuerUrl: https://dex.gazelle.example")

	in.Values = map[string]any{"global": map[string]any{"controlPlane": map[string]any{"oidc": map[string]any{"issuerUrl": "https://login.acme.example", "clientId": "acme"}}}}
	got, err = svc.CreateCluster(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, &ClusterTrust{Source: TrustValues, IssuerURL: "https://login.acme.example", ClientID: "acme"}, got.OIDC)
	assert.NotContains(t, clusterValuesOf(t, got), "dex.gazelle.example")

	_, plain := releasesService(t)
	got, err = plain.CreateCluster(ctx, dev01())
	require.NoError(t, err)
	assert.Equal(t, TrustNone, got.OIDC.Source)
	assert.Contains(t, got.OIDC.Note, "the cluster trusts no identity provider")
}

// clusterValuesOf is the values document of a create's ConfigMap manifest.
func clusterValuesOf(t *testing.T, got *WriteResult) string {
	t.Helper()
	for _, m := range got.Manifests {
		if m["kind"] == "ConfigMap" {
			v, _, _ := unstructured.NestedString(m, "data", compose.ValuesSecretKey)
			return v
		}
	}
	t.Fatal("no ConfigMap manifest")
	return ""
}

// TestCreateClusterRefusals: every refusal names its reason and comes before
// any write.
func TestCreateClusterRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*CreateClusterInput)
		want string
	}{
		"name too long":          {func(in *CreateClusterInput) { in.Name = "a-cluster-name-too-long" }, "DNS label of at most 20 characters"},
		"name taken":             {func(in *CreateClusterInput) { in.Name = "wc1" }, "cluster name wc1 is taken: cluster org-acme/wc1 exists"},
		"provider not offered":   {func(in *CreateClusterInput) { in.Provider = "vsphere" }, `provider "vsphere": create_cluster offers aws, azure, eks`},
		"preview release":        {func(in *CreateClusterInput) { in.Release = "37.0.0" }, "release aws-37.0.0 is preview: only an active release is offered"},
		"deprecated release":     {func(in *CreateClusterInput) { in.Release = "v34.0.0" }, "release aws-34.0.0 is deprecated"},
		"unknown release":        {func(in *CreateClusterInput) { in.Release = "99.0.0" }, "release aws-99.0.0 does not exist on this installation"},
		"unpublished tag":        {func(in *CreateClusterInput) { in.Provider, in.Identity = "azure", "" }, "release chart oci://gsoci.azurecr.io/charts/giantswarm/release-azure:36.0.0 is not published in the registry"},
		"no installation values": {func(in *CreateClusterInput) { in.Organization = "empty" }, "ConfigMap org-empty/cluster-app-installation-values does not exist"},
		"schema": {func(in *CreateClusterInput) {
			in.Values = map[string]any{"global": map[string]any{"metadata": map[string]any{"servicePriority": "urgent"}}}
		}, "/global/metadata/servicePriority: value must be one of 'highest', 'medium', 'lowest'"},
		"contradiction": {func(in *CreateClusterInput) {
			in.Values = map[string]any{"global": map[string]any{"metadata": map[string]any{"organization": "other"}}}
		}, "global.metadata.organization is other, but the organization argument sets it"},
		"commit mode without the GitHub App": {func(in *CreateClusterInput) { in.Mode = ModeCommit }, "mode commit (a pull request opened as you) is not offered by this server"},
	} {
		t.Run(name, func(t *testing.T) {
			l, svc := releasesService(t)
			log := recordWrites(t, l)
			in := dev01()
			tc.edit(&in)
			_, err := svc.CreateCluster(context.Background(), in)
			assertRefused(t, err, tc.want)
			assert.Empty(t, log.seen(), "every refusal comes before any write")
		})
	}
}

// TestCreateClusterRefusesAnUnboundOrganization: in an organization whose
// tenant ServiceAccount rbac-operator has not bound yet, the call refuses
// before any write, dry run included, naming the binding and the re-run; an
// organization with the binding passes.
func TestCreateClusterRefusesAnUnboundOrganization(t *testing.T) {
	l := newLab(t, "releases.yaml")
	svc := l.service(Config{Installation: "gazelle", TenantServiceAccount: compose.DefaultTenantServiceAccount}, WithChartReader(releaseCharts(t)))
	log := recordWrites(t, l)
	ctx := context.Background()
	for _, dryRun := range []bool{true, false} {
		in := dev01()
		in.Organization, in.DryRun = "fresh", dryRun
		_, err := svc.CreateCluster(ctx, in)
		assertRefused(t, err, "RoleBinding org-fresh/write-all-customer-sa does not exist yet: the organization's ServiceAccount automation")
		assertRefused(t, err, "re-run create_cluster then")
	}
	assert.Empty(t, log.seen(), "the refusal comes before any write")

	in := dev01()
	in.DryRun = true
	_, err := svc.CreateCluster(ctx, in)
	require.NoError(t, err)
}

// TestCreateClusterRefusesAGitOpsObject: an object of the cluster's names
// that someone else owns is never patched; the refusal names its owner.
func TestCreateClusterRefusesAGitOpsObject(t *testing.T) {
	l, svc := releasesService(t)
	ctx := context.Background()
	values := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "dev01-values", "namespace": "org-acme", "labels": map[string]any{labelKustomizeName: "flux-acme"}},
	}}
	_, err := l.installation.Resource(ConfigMapGVR).Namespace("org-acme").Create(ctx, values, metav1.CreateOptions{})
	require.NoError(t, err)
	log := recordWrites(t, l)
	_, err = svc.CreateCluster(ctx, dev01())
	assertRefused(t, err, "ConfigMap org-acme/dev01-values exists and is owned by GitOps (Flux Kustomization flux-acme)")
	assert.Empty(t, log.seen())
}
