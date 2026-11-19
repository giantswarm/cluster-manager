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

	oauth "github.com/giantswarm/mcp-oauth"
	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/giantswarm/mcp-oauth/storage/memory"
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

func (f *fakeIdP) config(downstream bool) OAuthConfig {
	return OAuthConfig{
		BaseURL:          "http://localhost:8080",
		Provider:         ProviderDex,
		DexIssuerURL:     f.issuer,
		DexClientID:      "agent-platform",
		DexClientSecret:  "lab-only",
		DexCAFile:        f.caFile,
		TrustedAudiences: []string{"agent-platform"},
		DownstreamOAuth:  downstream,
	}
}

func TestOAuthConfigValidation(t *testing.T) {
	dexOK := OAuthConfig{BaseURL: "https://mm.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}
	require.Error(t, OAuthConfig{}.Validate())
	require.Error(t, OAuthConfig{BaseURL: "http://mm.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate(), "plain http only on loopback")
	require.NoError(t, OAuthConfig{BaseURL: "http://localhost:8080", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate())
	require.NoError(t, dexOK.Validate())
	require.Error(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: ProviderDex}.Validate(), "dex needs issuer + client")
	require.Error(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: ProviderGoogle, GoogleClientID: "id"}.Validate(), "google needs the secret")
	require.NoError(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: ProviderGoogle, GoogleClientID: "id", GoogleClientSecret: "s", TrustedAudiences: []string{"id"}}.Validate())
	require.Error(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: "okta"}.Validate(), "unknown provider")
	bad := dexOK
	bad.TrustedAudiences = []string{"agent-platform", " "}
	require.Error(t, bad.Validate(), "empty audience")
}

func newTestOAuthRuntime(cfg OAuthConfig) *oauthRuntime {
	return &oauthRuntime{cfg: cfg, store: memory.New(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	o := newTestOAuthRuntime(OAuthConfig{DownstreamOAuth: true})
	var seen struct {
		id    *identity.Identity
		token string
		ok    bool
	}
	h := o.attachIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, seen.ok = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated("forwarded-id-token", forwardedUser))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the caller reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.id.Email)
	assert.Equal(t, "sub-admin", seen.id.Subject)
	assert.Equal(t, []string{"platform-admins"}, seen.id.Groups)
	assert.Equal(t, identity.SourceSSO, seen.id.Source)
	assert.Equal(t, "forwarded-id-token", seen.token, "downstream OAuth: the forwarded id_token is what the Kubernetes API will see")

	seen.ok = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated("forwarded-id-token", nil))
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
	o := newTestOAuthRuntime(OAuthConfig{DownstreamOAuth: true})
	o.server = &oauth.Server{Config: &oauth.ServerConfig{Issuer: "https://mm.example.com"}}
	issued := &providers.UserInfo{ID: "sub-admin", Email: "admin@lab.local", TokenSource: providers.TokenSourceOAuth}
	var token string
	h := o.attachIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ = identity.TokenFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticated("issued-access-token", issued))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`)
	assert.Empty(t, token)
}

// TestServerGuardsMCPButNotProbes wires OAuth through the assembled server:
// probes and the OAuth metadata stay open, the MCP endpoint demands a token.
func TestServerGuardsMCPButNotProbes(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := idp.config(false)
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

func TestOAuthServerConfig(t *testing.T) {
	rootCAs := x509.NewCertPool()
	cfg := OAuthConfig{BaseURL: "https://mm.example.com", TrustedAudiences: []string{"agent-platform", "kubernetes"}, AllowPrivateIPJWKSHosts: []string{"keys.example.com"}, AllowPublicClientRegistration: true}
	assert.Equal(t, &oauth.ServerConfig{
		Issuer:                        "https://mm.example.com",
		AllowRefreshTokenRotation:     true,
		AllowPublicClientRegistration: true,
		MaxClientsPerIP:               10,
		TrustedAudiences:              []string{"agent-platform", "kubernetes"},
		AllowPrivateIPJWKSHosts:       []string{"keys.example.com"},
		JWKSRootCAs:                   rootCAs,
	}, oauthServerConfig(cfg, rootCAs))
}
