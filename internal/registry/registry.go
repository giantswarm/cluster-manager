// Package registry reads Helm charts from an OCI registry the way Flux's
// source-controller does — the tag list of a repository, one chart archive
// by exact version — with nothing but the registry's anonymous pull flow: the
// bearer challenge the registry answers with is followed to its token
// endpoint, the token cached per scope. It exists so a tool can read what a
// chart *would* install (the serving presets the connectivity chart ships)
// before any release of it is on the cluster.
package registry

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
)

// Media types of a Helm chart pushed as an OCI artifact.
const (
	mediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeChartLayer  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	// maxChartBytes bounds one chart archive: the platform's charts are a
	// few hundred KiB; anything past this is not a chart we compose from.
	maxChartBytes = 32 << 20
	// tagsPage is the page size asked of the tag list; the registry may
	// answer fewer and link to the next page.
	tagsPage = 1000
	// tokenSlack ends a token's cached life early, before its expiry.
	tokenSlack = 30 * time.Second
	// defaultTokenLife applies when the token endpoint names no expiry.
	defaultTokenLife = 5 * time.Minute
)

// Ref is one repository of a registry, from an `oci://<host>/<path>` URL.
type Ref struct {
	Host string
	Path string
}

// ParseRef parses `oci://<host>/<repository path>`.
func ParseRef(chartURL string) (Ref, error) {
	u, err := url.Parse(chartURL)
	if err != nil {
		return Ref{}, fmt.Errorf("chart URL %q: %w", chartURL, err)
	}
	if u.Scheme != "oci" || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return Ref{}, fmt.Errorf("chart URL %q: want oci://<registry>/<repository>", chartURL)
	}
	return Ref{Host: u.Host, Path: strings.Trim(u.Path, "/")}, nil
}

// String is the ref as an `oci://` URL.
func (r Ref) String() string { return "oci://" + r.Host + "/" + r.Path }

// Chart is one chart archive, unpacked: its files by path below the chart's
// root directory (`values.yaml`, `files/model-serving/presets/x.yaml`).
type Chart struct {
	Ref     Ref
	Version string
	Files   map[string][]byte
}

// File is the content of one file of the chart, nil when the chart has none
// at that path.
func (c *Chart) File(name string) []byte { return c.Files[name] }

// Glob lists the chart's files below a directory, sorted by path.
func (c *Chart) Glob(dir string) []string {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	for name := range c.Files {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Client reads a registry anonymously. Zero value ready with
// http.DefaultClient; a Client is safe for concurrent use and caches every
// chart it read (a version is immutable) and every token until its expiry.
type Client struct {
	// HTTP is the client every request goes through; nil is
	// http.DefaultClient with a 30 s timeout.
	HTTP *http.Client

	mu     sync.Mutex
	tokens map[string]token
	charts sync.Map // Ref@version -> *Chart
}

type token struct {
	value   string
	expires time.Time
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Tags lists the repository's tags, every page followed.
func (c *Client) Tags(ctx context.Context, ref Ref) ([]string, error) {
	next := fmt.Sprintf("https://%s/v2/%s/tags/list?n=%d", ref.Host, ref.Path, tagsPage)
	var out []string
	for next != "" {
		resp, err := c.get(ctx, ref, next, "")
		if err != nil {
			return nil, fmt.Errorf("list tags of %s: %w", ref, err)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list tags of %s: %w", ref, err)
		}
		out = append(out, body.Tags...)
		next = nextPage(ref, link)
	}
	return out, nil
}

// nextPage resolves the `Link: <url>; rel="next"` header against the
// registry; empty when there is no next page.
func nextPage(ref Ref, link string) string {
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start < 0 || end <= start {
			continue
		}
		u, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		base := &url.URL{Scheme: "https", Host: ref.Host}
		return base.ResolveReference(u).String()
	}
	return ""
}

// Chart pulls one chart by exact version and unpacks it; read once per
// version.
func (c *Client) Chart(ctx context.Context, ref Ref, version string) (*Chart, error) {
	key := ref.String() + "@" + version
	if cached, ok := c.charts.Load(key); ok {
		return cached.(*Chart), nil
	}
	chart, err := c.pull(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	c.charts.Store(key, chart)
	return chart, nil
}

func (c *Client) pull(ctx context.Context, ref Ref, version string) (*Chart, error) {
	resp, err := c.get(ctx, ref, fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Host, ref.Path, url.PathEscape(version)), mediaTypeOCIManifest)
	if err != nil {
		return nil, fmt.Errorf("pull %s %s: %w", ref, version, err)
	}
	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("pull %s %s: manifest: %w", ref, version, err)
	}
	digest := ""
	for _, l := range manifest.Layers {
		if l.MediaType == mediaTypeChartLayer {
			digest = l.Digest
			break
		}
	}
	if digest == "" {
		return nil, fmt.Errorf("pull %s %s: the manifest carries no %s layer: not a Helm chart", ref, version, mediaTypeChartLayer)
	}
	resp, err = c.get(ctx, ref, fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Host, ref.Path, digest), "")
	if err != nil {
		return nil, fmt.Errorf("pull %s %s: chart layer: %w", ref, version, err)
	}
	defer func() { _ = resp.Body.Close() }()
	files, err := unpack(io.LimitReader(resp.Body, maxChartBytes+1))
	if err != nil {
		return nil, fmt.Errorf("pull %s %s: chart layer %s: %w", ref, version, digest, err)
	}
	return &Chart{Ref: ref, Version: version, Files: files}, nil
}

// unpack reads a chart archive (a gzipped tar with one root directory) into
// files keyed by their path below that directory.
func unpack(r io.Reader) (map[string][]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("not gzip: %w", err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(hdr.Name)
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[i+1:]
		} else {
			continue // a file at the archive's root is not the chart's
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if total += int64(len(data)); total > maxChartBytes {
			return nil, fmt.Errorf("larger than %d MiB", maxChartBytes>>20)
		}
		files[name] = data
	}
	return files, nil
}

// get performs one GET with the repository's pull token, following the
// bearer challenge once when the registry answers 401.
func (c *Client) get(ctx context.Context, ref Ref, u, accept string) (*http.Response, error) {
	scope := "repository:" + ref.Path + ":pull"
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if t := c.token(ref.Host, scope); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			challenge := resp.Header.Get("Www-Authenticate")
			_ = resp.Body.Close()
			if err := c.authenticate(ctx, ref.Host, scope, challenge); err != nil {
				return nil, err
			}
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%s: HTTP %d %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}

func (c *Client) token(host, scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[host+" "+scope]
	if !ok || time.Now().After(t.expires) {
		return ""
	}
	return t.value
}

// authenticate follows a `Bearer realm=…,service=…` challenge to the
// registry's token endpoint anonymously for the repository's pull scope.
func (c *Client) authenticate(ctx context.Context, host, scope, challenge string) error {
	params := bearerParams(challenge)
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("%s answered 401 without a bearer challenge (%q)", host, challenge)
	}
	q := url.Values{"scope": {scope}}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("token for %s: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token for %s: HTTP %d", host, resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("token for %s: %w", host, err)
	}
	value := body.AccessToken
	if value == "" {
		value = body.Token
	}
	if value == "" {
		return fmt.Errorf("token for %s: the token endpoint answered no token", host)
	}
	life := defaultTokenLife
	if body.ExpiresIn > 0 {
		life = time.Duration(body.ExpiresIn) * time.Second
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]token{}
	}
	c.tokens[host+" "+scope] = token{value: value, expires: time.Now().Add(life - tokenSlack)}
	return nil
}

var bearerParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// bearerParams parses the key="value" pairs of a Bearer challenge.
func bearerParams(challenge string) map[string]string {
	out := map[string]string{}
	for _, m := range bearerParam.FindAllStringSubmatch(challenge, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// Resolve picks the newest tag that satisfies a semver constraint — Flux's
// `spec.ref.semver` — after an optional regexp filter (`spec.ref.semverFilter`)
// on the raw tags. Tags that are no semantic version are skipped, as
// source-controller skips them. An empty result is an error naming the
// constraint.
func Resolve(tags []string, constraint, filter string) (string, error) {
	rng, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("version range %q: %w", constraint, err)
	}
	var re *regexp.Regexp
	if filter != "" {
		if re, err = regexp.Compile(filter); err != nil {
			return "", fmt.Errorf("semver filter %q: %w", filter, err)
		}
	}
	var (
		best    *semver.Version
		bestTag string
	)
	for _, tag := range tags {
		if re != nil && !re.MatchString(tag) {
			continue
		}
		v, err := semver.StrictNewVersion(tag)
		if err != nil {
			continue
		}
		if !rng.Check(v) {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best, bestTag = v, tag
		}
	}
	if best == nil {
		if filter != "" {
			return "", fmt.Errorf("no tag satisfies %q with filter %q among %d tag(s)", constraint, filter, len(tags))
		}
		return "", fmt.Errorf("no tag satisfies %q among %d tag(s)", constraint, len(tags))
	}
	return bestTag, nil
}
