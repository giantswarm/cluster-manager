package compose

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// RenderOracleChartVersion is the agent-platform chart release the render
// oracle (TestSliceRendersThroughChart) puts the composed slice through: the
// release the platform ran where the slice was last proved (gazelle, proof 1
// of giantswarm/giantswarm#37639). Move it when a platform release adds a
// render guard the slice must satisfy; SLICE_RENDER_CHART_VERSION overrides
// it for one run.
const RenderOracleChartVersion = "4.28.10"

// TestSliceRendersThroughChart is the render oracle
// (giantswarm/cluster-manager#30): the composed slice values go through the
// chart the way the installation's Flux takes them — `helm template` of the
// agent-platform chart at the pinned version, then the connectivity child's
// HelmRelease values through the connectivity chart at the version the meta
// chart pins — so a chart-side validation guard fails here, not on an
// installation. On gazelle the own-cluster slice named an in-cluster JWKS host
// without gateway.jwksEgress: the connectivity child failed its render
// (validate.yaml:46) while the meta release read Ready. Needs helm on PATH (the
// CI image carries it) and the registry: skipped under -short, and in CI a
// missing helm is a failure, not a skip.
func TestSliceRendersThroughChart(t *testing.T) {
	if testing.Short() {
		t.Skip("the render oracle pulls the charts from the registry")
	}
	bin, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not on PATH: the render oracle cannot run in CI")
		}
		t.Skip("helm is not on PATH")
	}
	version := RenderOracleChartVersion
	if v := os.Getenv("SLICE_RENDER_CHART_VERSION"); v != "" {
		version = v
	}
	h := newHelm(t, bin)
	meta := h.pull(t, SliceChartURL, version)

	own := wc1()
	own.Name, own.Namespace, own.Organization = "gazelle", "org-giantswarm", "giantswarm"
	cases := []struct {
		name    string
		cluster Cluster
		spec    SliceSpec
		jwks    string
	}{
		{"own-cluster", own, SliceSpec{OwnCluster: true, Platform: platform(), Pool: "gpu-l4", CertificateIssuer: DefaultCertificateIssuer}, "dex.giantswarm.svc.cluster.local"},
		{"workload", wc1(), SliceSpec{Platform: platform(), Pool: "gpu-l4", CertificateIssuer: DefaultCertificateIssuer}, "dex.gazelle.example.io"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values, err := SliceValues(tc.cluster, tc.spec)
			require.NoError(t, err)
			rendered := objects(t, h.template(t, meta, values))
			child := find(t, rendered, "HelmRelease", "agent-platform-connectivity")
			source := find(t, rendered, "OCIRepository", "agent-platform-connectivity")
			childURL, _, _ := unstructured.NestedString(source.Object, "spec", "url")
			childVersion, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "semver")
			require.NotEmpty(t, childURL, "the meta chart names the connectivity chart")
			require.NotEmpty(t, childVersion, "the meta chart pins the connectivity chart's version")
			childValues, _, _ := unstructured.NestedMap(child.Object, "spec", "values")
			connectivity := h.template(t, h.pull(t, childURL, childVersion), childValues)
			find(t, objects(t, connectivity), "Gateway", "models")
			assert.Contains(t, string(connectivity), ModelsHost(SliceDomain(tc.cluster, tc.spec)), "the models Gateway answers at the host the slice composed")
			assert.Contains(t, string(connectivity), tc.jwks, "the JWKS source the slice composed reaches the rendered JWT policy")
		})
	}
}

// helmRunner runs the helm binary with its cache, config and data under a
// temporary home, so a run reads no user configuration and leaves nothing
// behind; a chart is pulled once per URL and version.
type helmRunner struct {
	bin    string
	env    []string
	charts string
	pulled map[string]string
}

func newHelm(t *testing.T, bin string) *helmRunner {
	t.Helper()
	home := t.TempDir()
	return &helmRunner{
		bin: bin,
		env: append(os.Environ(),
			"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
			"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
			"HELM_DATA_HOME="+filepath.Join(home, "data"),
			"HELM_REGISTRY_CONFIG="+filepath.Join(home, "config", "registry.json"),
		),
		charts: t.TempDir(),
		pulled: map[string]string{},
	}
}

// pull fetches a chart from the registry and answers its archive.
func (h *helmRunner) pull(t *testing.T, url, version string) string {
	t.Helper()
	key := url + "@" + version
	if archive, ok := h.pulled[key]; ok {
		return archive
	}
	dir := filepath.Join(h.charts, fmt.Sprint(len(h.pulled)))
	require.NoError(t, os.MkdirAll(dir, 0o750), "helm pull writes into an existing destination only")
	h.run(t, "pull", url, "--version", version, "--destination", dir)
	archives, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	require.NoError(t, err)
	require.Len(t, archives, 1, "one chart archive pulled for %s", key)
	h.pulled[key] = archives[0]
	return archives[0]
}

// template renders a chart archive with the values, as helm-controller
// would; a render error fails the test with helm's message.
func (h *helmRunner) template(t *testing.T, archive string, values map[string]any) []byte {
	t.Helper()
	raw, err := yaml.Marshal(values)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	return h.run(t, "template", "slice", archive, "--values", file)
}

func (h *helmRunner) run(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(h.bin, args...) //nolint:gosec // the helm binary from PATH with the test's own arguments
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Run(), "helm %s:\n%s", strings.Join(args, " "), stderr.String())
	return stdout.Bytes()
}

// objects decodes a rendered manifest stream; a document of comments alone is
// skipped.
func objects(t *testing.T, rendered []byte) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for _, doc := range strings.Split(string(rendered), "\n---") {
		u := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &u.Object), doc)
		if u.GetKind() != "" {
			out = append(out, u)
		}
	}
	return out
}

// find is the rendered object of a kind and name; none fails the test.
func find(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s %q rendered", kind, name)
	return nil
}
