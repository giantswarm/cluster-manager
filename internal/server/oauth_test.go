package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/oauthconfig"
	"github.com/giantswarm/mcp-oauth/providers"
	oauthserver "github.com/giantswarm/mcp-oauth/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/api"
	"github.com/giantswarm/cluster-manager/internal/identity"
)

// fakeIdP is a minimal OIDC issuer on https://localhost serving the discovery
// document, enough for mcp-oauth's Dex provider to boot — the shape of the
// platform's Dex.
type fakeIdP struct {
	issuer string
	caFile string
	srv    *httptest.Server
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/dex/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.issuer,
			"authorization_endpoint":                idp.issuer + "/auth",
			"token_endpoint":                        idp.issuer + "/token",
			"userinfo_endpoint":                     idp.issuer + "/userinfo",
			"jwks_uri":                              idp.issuer + "/keys",
			"response_types_supported":              []string{"code"},
			"scopes_supported":                      []string{"openid", "email", "profile", "groups", "offline_access"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		})
	})

	// A hostname that resolves to loopback (localhost, like the agentlab Dex);
	// httptest's certificate has no localhost SAN, so mint one.
	srv := httptest.NewUnstartedServer(mux)
	cert := selfSignedLocalhost(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	idp.srv = srv
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	require.NoError(t, err)
	idp.issuer = "https://localhost:" + port + "/dex"

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	idp.caFile = caPath
	return idp
}

func selfSignedLocalhost(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (f *fakeIdP) config(t *testing.T, downstream bool) OAuthConfig {
	t.Helper()
	return OAuthConfig{Settings: oauthSettings(t, dexEnvironment(f.issuer, f.caFile)), DownstreamOAuth: downstream}
}

// dexEnvironment is the OAUTH_* configuration of a Dex at issuer whose
// forwarded id_tokens for the platform client are accepted.
func dexEnvironment(issuer, caFile string) map[string]string {
	return map[string]string{
		"OAUTH_ISSUER":                              "http://localhost:8080",
		"OAUTH_PROVIDER":                            "dex",
		"OAUTH_DEX_ISSUER_URL":                      issuer,
		"OAUTH_DEX_CLIENT_ID":                       "agent-platform",
		"OAUTH_DEX_CLIENT_SECRET":                   "lab-only",
		"OAUTH_DEX_CA_FILE":                         caFile,
		"OAUTH_TRUSTED_AUDIENCES":                   "agent-platform",
		"OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true",
	}
}

// oauthSettings reads mcp-oauth's settings from environment the way the
// serve command does.
func oauthSettings(t *testing.T, environment map[string]string) oauthconfig.Settings {
	t.Helper()
	for name, value := range environment {
		t.Setenv(name, value)
	}
	settings, err := oauthconfig.SettingsFromEnv()
	require.NoError(t, err)
	return settings
}

// googleSettings is a Google provider configuration for a server at
// https://manager.example.test.
func googleSettings(t *testing.T) oauthconfig.Settings {
	t.Helper()
	return oauthSettings(t, map[string]string{
		"OAUTH_ISSUER": "https://manager.example.test", "OAUTH_PROVIDER": "google", "OAUTH_GOOGLE_CLIENT_ID": "client",
		"OAUTH_GOOGLE_CLIENT_SECRET": "test-secret", "OAUTH_GOOGLE_REDIRECT_URL": "https://manager.example.test/oauth/callback",
	})
}

func newTestOAuthRuntime(cfg OAuthConfig) *oauthRuntime {
	return &oauthRuntime{downstreamOAuth: cfg.DownstreamOAuth, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// authenticated is a request mcp-oauth has validated: its user info is on the
// context, as ValidateToken leaves it.
func authenticated(bearer string, info *providers.UserInfo) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if info != nil {
		req = req.WithContext(handler.ContextWithUserInfo(req.Context(), info))
	}
	return req
}

var forwardedUser = &providers.UserInfo{
	ID: "sub-admin", Email: "admin@lab.local", Name: "Lab Admin", Groups: []string{"platform-admins"},
	TokenSource: providers.TokenSourceSSO,
}

// TestAttachIdentityForwardedIDToken is the platform path after validation:
// muster (or the portal through the gateway) forwarded the IdP id_token for
// the platform client; the request carries the caller and, with downstream
// OAuth, the caller's token.
func TestAttachIdentityForwardedIDToken(t *testing.T) {
	identityProvider := newPrivateIdP(t, "")
	configuration := identityProvider.config(t)
	configuration.DownstreamOAuth = true
	o, err := newOAuth(configuration, "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.WithoutCancel(t.Context())) })
	forwardedToken := identityProvider.idToken(t, []string{"agent-platform"})
	var seen struct {
		id    *identity.Identity
		token string
		ok    bool
	}
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, seen.ok = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request.Header.Set("Authorization", "bEaReR "+forwardedToken)
	h.ServeHTTP(rec, request)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the caller reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.id.Email)
	assert.Equal(t, "sub-admin", seen.id.Subject)
	assert.Equal(t, []string{"platform-admins"}, seen.id.Groups)
	assert.Equal(t, identity.SourceSSO, seen.id.Source)
	assert.Equal(t, forwardedToken, seen.token, "downstream OAuth: the forwarded id_token is what the Kubernetes API will see")

	seen.ok = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated("untrusted-token", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no validated user, no caller")
	assert.False(t, seen.ok)
}

func TestAttachIdentityDownstreamOffKeepsTheServiceAccount(t *testing.T) {
	o := newTestOAuthRuntime(OAuthConfig{})
	var caller string
	var hasToken bool
	h := o.attachIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller = identity.Caller(r.Context())
		_, hasToken = identity.TokenFromContext(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated("forwarded-id-token", forwardedUser))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "admin@lab.local", caller, "the caller is known and attributed")
	assert.False(t, hasToken, "but nothing is presented to the Kubernetes API")
}

// TestAttachIdentityIssuedTokenNeedsStoredIDToken: for a token this server
// issued, the provider's id_token stored next to it is what the Kubernetes API
// sees; without one the request is refused.
func TestAttachIdentityIssuedTokenNeedsStoredIDToken(t *testing.T) {
	configuration := OAuthConfig{Settings: googleSettings(t), DownstreamOAuth: true}
	o, err := newOAuth(configuration, "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.WithoutCancel(t.Context())) })
	var token string
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ = identity.TokenFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated("unknown-access-token", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`)
	assert.Empty(t, token)
}

// TestServerGuardsMCPButNotProbes wires OAuth through the assembled server:
// probes and the OAuth metadata stay open, the MCP endpoint demands a token.
func TestServerGuardsMCPButNotProbes(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := idp.config(t, false)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true, OAuth: &cfg}, api.NewMCPServer(nil, "test"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status := func(method, path, bearer string) int {
		req, err := http.NewRequest(method, ts.URL+path, nil)
		require.NoError(t, err)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/healthz", ""))
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/readyz", ""))
	assert.Equal(t, http.StatusUnauthorized, status(http.MethodPost, "/mcp", ""), "MCP needs a token")
	assert.Equal(t, http.StatusNotFound, status(http.MethodGet, "/api/v1/backend", ""), "there is no REST API")
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/.well-known/oauth-authorization-server", ""), "metadata stays public")
}

func TestProtectedResources(t *testing.T) {
	advertised := []string{"https://dex.example.com"}
	configuration := &oauthserver.Config{Issuer: "https://cm.example.com", ResourceMetadataByPath: map[string]oauthserver.ProtectedResourceConfig{
		"/": {ResourceIdentifier: "https://cm.example.com", AuthorizationServers: advertised},
	}}
	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{
		"/":    {ResourceIdentifier: "https://cm.example.com", AuthorizationServers: advertised},
		"/mcp": {AuthorizationServers: advertised},
	}, protectedResources(configuration, "/mcp"), "an added path advertises the main resource's servers")
	assert.Len(t, configuration.ResourceMetadataByPath, 1, "the configuration is not changed")

	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{"/mcp": {}},
		protectedResources(&oauthserver.Config{Issuer: "https://cm.example.com"}, "/mcp"), "nothing advertised: the server's own issuer")

	trailing := &oauthserver.Config{Issuer: "https://cm.example.com", ResourceMetadataByPath: map[string]oauthserver.ProtectedResourceConfig{
		"/mcp/": {ResourceIdentifier: "https://cm.example.com/mcp/", ScopesSupported: []string{"clusters"}},
	}}
	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{"/mcp/": {ResourceIdentifier: "https://cm.example.com/mcp/", ScopesSupported: []string{"clusters"}}},
		protectedResources(trailing, "/mcp"), "a configured path with a trailing slash describes the MCP endpoint")
}
