package server

import (
	"bytes"
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
	"github.com/giantswarm/cluster-manager/internal/idtoken"
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

// TestForwardedIDTokenBecomesTheUser is the platform path: muster (or the
// portal through the gateway) forwards the ID token for the platform client;
// once mcp-oauth has validated it, the request carries the user and, with
// downstream OAuth, their ID token.
func TestForwardedIDTokenBecomesTheUser(t *testing.T) {
	identityProvider := newPrivateIdP(t, "")
	configuration := identityProvider.config(t)
	configuration.DownstreamOAuth = true
	o, err := newOAuth(configuration, "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.WithoutCancel(t.Context())) })
	forwardedToken := identityProvider.idToken(t, []string{"agent-platform"})
	var seen struct {
		user  *providers.UserInfo
		token string
		ok    bool
	}
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.user, seen.ok = handler.UserInfoFromContext(r.Context())
		seen.token, _ = idtoken.FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request.Header.Set("Authorization", "bEaReR "+forwardedToken)
	h.ServeHTTP(rec, request)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the user reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.user.Email)
	assert.Equal(t, "sub-admin", seen.user.ID)
	assert.Equal(t, []string{"platform-admins"}, seen.user.Groups)
	assert.True(t, seen.user.IsSSO(), "a forwarded id_token")
	assert.Equal(t, forwardedToken, seen.token, "downstream OAuth: the forwarded id_token is what the Kubernetes API will see")

	seen.ok = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated("untrusted-token", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no validated user, no caller")
	assert.False(t, seen.ok)

	// No validated user, no ID token to attach.
	rec = httptest.NewRecorder()
	attachIDToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestDownstreamOffKeepsTheServiceAccount(t *testing.T) {
	identityProvider := newPrivateIdP(t, "")
	o, err := newOAuth(identityProvider.config(t), "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.WithoutCancel(t.Context())) })
	var attributed string
	var hasToken bool
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, ok := handler.UserInfoFromContext(r.Context()); ok {
			attributed = user.Email
		}
		_, hasToken = idtoken.FromContext(r.Context())
	}))
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+identityProvider.idToken(t, []string{"agent-platform"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "admin@lab.local", attributed, "the user is known and attributed")
	assert.False(t, hasToken, "but nothing is presented to the Kubernetes API")
}

// TestDownstreamNeedsAnIDToken: a bearer mcp-oauth validates from a trusted
// issuer that is not the OpenID Provider is no ID token, so under downstream
// OAuth there is nothing to present to the Kubernetes API: the request is
// refused and the handler never runs. Without downstream OAuth the same
// bearer is admitted.
func TestDownstreamNeedsAnIDToken(t *testing.T) {
	identityProvider := newPrivateIdP(t, "")
	bearer := identityProvider.idToken(t, []string{"kubernetes"})

	configuration := identityProvider.trustedIssuerConfig(t, true, privateIssuerHost)
	configuration.Settings.TrustedIssuers[0].OpenIDProvider = false
	o, err := newOAuth(configuration, "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.WithoutCancel(t.Context())) })
	ran := false
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated(bearer, nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	require.False(t, ran, "the handler never runs without an ID token to present")

	configuration.DownstreamOAuth = false
	code, caller := callThroughChain(t, configuration, bearer)
	require.Equal(t, http.StatusNoContent, code, "the bearer itself is valid")
	require.Equal(t, "admin@lab.local", caller)
}

func TestLogUserNamesTheUserWithoutProfileData(t *testing.T) {
	var logs bytes.Buffer
	o := &oauthRuntime{log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	user := &providers.UserInfo{Issuer: "https://dex.example.test", ID: "sub-admin", Email: "admin@lab.local", TokenSource: providers.TokenSourceSSO}
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request = request.WithContext(handler.ContextWithUserInfo(request.Context(), user))

	o.logUser(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(httptest.NewRecorder(), request)

	var line map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &line))
	require.Equal(t, map[string]any{"issuer": "https://dex.example.test", "subject": "sub-admin"}, line["user"])
	require.Equal(t, string(providers.TokenSourceSSO), line["token_source"])
	require.NotContains(t, logs.String(), "admin@lab.local")
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
