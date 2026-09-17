package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Archive builds a chart archive the way `helm package` does: a gzipped tar
// with the chart's files below one root directory named after the chart.
func Archive(t *testing.T, chart string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: chart + "/" + name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// fakeRegistry is an OCI registry with anonymous bearer authentication: a
// 401 challenge on every unauthenticated request, a token endpoint, a
// paginated tag list, manifests and blobs served through a redirect the way
// the fleet's registry hands blobs to its storage.
type fakeRegistry struct {
	t        *testing.T
	repos    map[string]map[string][]byte // path -> version -> archive
	requests atomic.Int32
	tokens   atomic.Int32
	server   *httptest.Server
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{t: t, repos: map[string]map[string][]byte{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.server.URL, "https://") }

func (f *fakeRegistry) client() *Client { return &Client{HTTP: f.server.Client()} }

func (f *fakeRegistry) add(repo, version string, archive []byte) {
	if f.repos[repo] == nil {
		f.repos[repo] = map[string][]byte{}
	}
	f.repos[repo][version] = archive
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f *fakeRegistry) handle(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.URL.Path == "/oauth2/token" {
		f.tokens.Add(1)
		assert.Equal(f.t, "fake", r.URL.Query().Get("service"))
		assert.Contains(f.t, r.URL.Query().Get("scope"), ":pull")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "anon-" + r.URL.Query().Get("scope"), "expires_in": 3600})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/storage/") {
		// The blob's storage, reached through the redirect (the fleet's
		// registry hands blobs to another host, where Go's client drops the
		// Authorization header; this fake is one host, so it is kept).
		digest := strings.TrimPrefix(r.URL.Path, "/storage/")
		for _, versions := range f.repos {
			for _, archive := range versions {
				if digestOf(archive) == digest {
					_, _ = w.Write(archive)
					return
				}
			}
		}
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer anon-") {
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/oauth2/token",service="fake",scope="repository:x:pull"`, f.server.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case strings.HasSuffix(rest, "/tags/list"):
		repo := strings.TrimSuffix(rest, "/tags/list")
		versions := f.repos[repo]
		tags := make([]string, 0, len(versions))
		for v := range versions {
			tags = append(tags, v)
		}
		// Two pages: the first tag alone, then the rest.
		if r.URL.Query().Get("last") == "" && len(tags) > 1 {
			w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=1000&last=%s>; rel="next"`, repo, tags[0]))
			tags = tags[:1]
		} else if last := r.URL.Query().Get("last"); last != "" {
			rest := []string{}
			for _, tag := range tags {
				if tag != last {
					rest = append(rest, tag)
				}
			}
			tags = rest
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
	case strings.Contains(rest, "/manifests/"):
		repo, version, _ := strings.Cut(rest, "/manifests/")
		archive, ok := f.repos[repo][version]
		if !ok {
			http.NotFound(w, r)
			return
		}
		assert.Equal(f.t, mediaTypeOCIManifest, r.Header.Get("Accept"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 2,
			"config":        map[string]any{"mediaType": "application/vnd.cncf.helm.config.v1+json", "digest": "sha256:0", "size": 1},
			"layers": []map[string]any{
				{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": "sha256:1", "size": 1},
				{"mediaType": mediaTypeChartLayer, "digest": digestOf(archive), "size": len(archive)},
			},
		})
	case strings.Contains(rest, "/blobs/"):
		_, digest, _ := strings.Cut(rest, "/blobs/")
		http.Redirect(w, r, f.server.URL+"/storage/"+digest, http.StatusTemporaryRedirect) //nolint:gosec // the fake registry hands blobs to its own storage path, as the fleet's does
	default:
		http.NotFound(w, r)
	}
}

// TestClientPullsThroughTheChallenge: the first request meets the 401
// challenge, the token endpoint is asked once for the repository's pull
// scope, the paginated tag list is read whole, the chart's layer is followed
// through the redirect and unpacked below its root directory; a second read
// of the same version is served from the cache.
func TestClientPullsThroughTheChallenge(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.add("charts/acme/meta", "1.2.3", Archive(t, "meta", map[string]string{"Chart.yaml": "name: meta\n", "values.yaml": "a: 1\n", "files/x/one.yaml": "one", "files/x/two.yaml": "two", "files/y.txt": "y"}))
	reg.add("charts/acme/meta", "1.2.4", Archive(t, "meta", map[string]string{"Chart.yaml": "name: meta\n"}))
	reg.add("charts/acme/meta", "2.0.0-dev.1", Archive(t, "meta", map[string]string{"Chart.yaml": "name: meta\n"}))
	c := reg.client()
	ref := Ref{Host: reg.host(), Path: "charts/acme/meta"}
	ctx := context.Background()

	tags, err := c.Tags(ctx, ref)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"1.2.3", "1.2.4", "2.0.0-dev.1"}, tags)
	assert.Equal(t, int32(1), reg.tokens.Load(), "one token for the repository")

	chart, err := c.Chart(ctx, ref, "1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", chart.Version)
	assert.Equal(t, "a: 1\n", string(chart.File("values.yaml")))
	assert.Equal(t, []string{"files/x/one.yaml", "files/x/two.yaml"}, chart.Glob("files/x"))
	assert.Nil(t, chart.File("meta/values.yaml"), "paths are below the chart's root directory")
	assert.Equal(t, int32(1), reg.tokens.Load(), "the token is reused")

	before := reg.requests.Load()
	again, err := c.Chart(ctx, ref, "1.2.3")
	require.NoError(t, err)
	assert.Same(t, chart, again)
	assert.Equal(t, before, reg.requests.Load(), "a version is immutable: read once")

	_, err = c.Chart(ctx, ref, "9.9.9")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
}

// TestParseRef: the oci:// form and nothing else.
func TestParseRef(t *testing.T) {
	ref, err := ParseRef("oci://gsoci.azurecr.io/charts/giantswarm/agent-platform")
	require.NoError(t, err)
	assert.Equal(t, Ref{Host: "gsoci.azurecr.io", Path: "charts/giantswarm/agent-platform"}, ref)
	assert.Equal(t, "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform", ref.String())
	for _, bad := range []string{"https://gsoci.azurecr.io/charts", "oci://gsoci.azurecr.io", "oci:///charts", "::"} {
		_, err := ParseRef(bad)
		assert.Error(t, err, bad)
	}
}

// TestResolve picks the newest tag in the range the way source-controller
// does: non-semver tags skipped, prereleases only through a range that
// admits them or a filter that selects them.
func TestResolve(t *testing.T) {
	tags := []string{"4.28.10", "4.29.1", "4.30.0", "4.30.1-dev.feat.2026-09-13.h1", "3.9.0", "latest", "5.0.0", "v4.31.0"}
	got, err := Resolve(tags, ">=4.0.0 <5.0.0", "")
	require.NoError(t, err)
	assert.Equal(t, "4.30.0", got)

	got, err = Resolve(tags, ">=4.0.0-0 <5.0.0-0", `-dev\.`)
	require.NoError(t, err)
	assert.Equal(t, "4.30.1-dev.feat.2026-09-13.h1", got, "the filter selects the dev channel, a range with prerelease bounds admits it (the library's rule, Flux's too)")
	_, err = Resolve(tags, ">=4.0.0 <5.0.0", `-dev\.`)
	require.Error(t, err, "a range without prerelease bounds admits no prerelease, whatever the filter selects")

	got, err = Resolve(tags, "4.29.1", "")
	require.NoError(t, err)
	assert.Equal(t, "4.29.1", got, "an exact pin")

	_, err = Resolve(tags, ">=6.0.0", "")
	require.Error(t, err)
	assert.Equal(t, `no tag satisfies ">=6.0.0" among 8 tag(s)`, err.Error())
	_, err = Resolve(tags, "not a range", "")
	require.Error(t, err)
	_, err = Resolve(tags, ">=4.0.0", "(")
	require.Error(t, err)
}

// TestBearerParams parses the challenge the fleet's registry answers.
func TestBearerParams(t *testing.T) {
	p := bearerParams(`Bearer realm="https://gsoci.azurecr.io/oauth2/token",service="gsoci.azurecr.io",scope="repository:charts/giantswarm/agent-platform:metadata_read"`)
	assert.Equal(t, "https://gsoci.azurecr.io/oauth2/token", p["realm"])
	assert.Equal(t, "gsoci.azurecr.io", p["service"])
	assert.Equal(t, "", nextPage(Ref{Host: "h"}, ""))
	assert.Equal(t, "https://h/v2/r/tags/list?last=x&n=1000", nextPage(Ref{Host: "h"}, `</v2/r/tags/list?last=x&n=1000>; rel="next"`))
}
