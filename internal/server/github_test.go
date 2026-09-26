package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/api"
	"github.com/giantswarm/cluster-manager/internal/identity"
)

// fakeGitHub answers GET /user for the one token it knows and counts calls.
func fakeGitHub(t *testing.T, token, login string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"login":"` + login + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestGitHubPinCarriesBothTokens is the App-pinned commit registration: the bearer
// is the person's App user token, verified with GET /user once per cache
// lifetime and kept for commit mode; the forwarded ID token in
// X-Muster-Id-Token is validated as a forwarded bearer is without the pin and
// becomes the caller and the Kubernetes token, so apply mode is unchanged.
func TestGitHubPinCarriesBothTokens(t *testing.T) {
	idp := newFakeIdP(t)
	gh, calls := fakeGitHub(t, "ghu_person", "jane")
	cfg := idp.config(true)
	cfg.GitHub = &GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-cluster-manager", Registration: "cluster-manager-commit", APIURL: gh.URL}
	o, err := newOAuth(cfg, "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var seen struct {
		id    *identity.Identity
		token string
		gh    *identity.GitHub
	}
	h := o.protectCommit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, _ = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		seen.gh, _ = identity.GitHubFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(bearer, idToken string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/commit/mcp", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if idToken != "" {
			req.Header.Set(ForwardedIdentityHeader, idToken)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	forwarded := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))

	rec := call("ghu_person", forwarded)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "admin@lab.local", seen.id.Email, "the caller is the person the ID token names")
	assert.Equal(t, forwarded, seen.token, "the Kubernetes API sees the forwarded ID token")
	assert.Equal(t, &identity.GitHub{Login: "jane", Token: "ghu_person"}, seen.gh, "commit mode gets the App user token")

	require.Equal(t, http.StatusNoContent, call("ghu_person", forwarded).Code)
	assert.Equal(t, 1, *calls, "a verified bearer is cached")

	rec = call("ghu_person", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "auth.forwardIdentity: true")

	rec = call("ghu_revoked", forwarded)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "core_auth_login server=cluster-manager-commit")

	rec = call("", forwarded)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	assert.Equal(t, http.StatusUnauthorized, call("ghu_person", idp.idToken(t, []string{"someone-else"}, time.Now().Add(30*time.Minute))).Code, "the ID token is validated as without the pin")
}

func TestGitHubPinValidation(t *testing.T) {
	assert.Error(t, GitHubPin{}.Validate())
	assert.Error(t, GitHubPin{AuthorizationServer: "github.com/apps/x", Registration: "cluster-manager-commit"}.Validate())
	assert.Error(t, GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-cluster-manager"}.Validate(), "the refusals name the commit registration")
	assert.NoError(t, GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-cluster-manager", Registration: "cluster-manager-commit"}.Validate())
}

// TestGitHubPinServesTwoRegistrations wires the pin through the assembled
// server (giantswarm/cluster-manager#120): the main path is the registration
// that forwards the IdP token, admitting a caller without the App's consent,
// with every tool; the commit path wants the App user token and the
// forwarded ID token, and serves the tools with a commit mode.
func TestGitHubPinServesTwoRegistrations(t *testing.T) {
	idp := newFakeIdP(t)
	gh, _ := fakeGitHub(t, "ghu_person", "jane")
	cfg := idp.config(true)
	cfg.GitHub = &GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-cluster-manager", Registration: "cluster-manager-commit", APIURL: gh.URL}
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true, OAuth: &cfg}, api.NewMCPServer(nil, "test"), api.NewCommitMCPServer(nil, "test"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	forwarded := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))
	call := func(path, bearer, idToken string, body string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if idToken != "" {
			req.Header.Set(ForwardedIdentityHeader, idToken)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

	code, body := call("/mcp", forwarded, "", initialize)
	assert.Equal(t, http.StatusOK, code, "the main path admits the forwarded IdP token alone: %s", body)
	code, body = call("/mcp", "", "", initialize)
	assert.Equal(t, http.StatusUnauthorized, code, "the main path still wants the IdP token: %s", body)

	code, body = call("/commit/mcp", forwarded, "", initialize)
	assert.Equal(t, http.StatusUnauthorized, code, "the commit path wants the App user token: %s", body)
	assert.Contains(t, body, "no "+ForwardedIdentityHeader+" header", "the forwarded IdP token as the bearer is not the App's")
	code, body = call("/commit/mcp", "ghu_revoked", forwarded, initialize)
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Contains(t, body, "core_auth_login server=cluster-manager-commit")
	code, body = call("/commit/mcp", "ghu_person", forwarded, initialize)
	assert.Equal(t, http.StatusOK, code, "the commit path admits the App user token with the forwarded ID token: %s", body)

	// The commit server without the pin, or the pin without it, is a
	// wiring error.
	plain := idp.config(true)
	_, err = New(Config{MCPEnabled: true, OAuth: &plain}, api.NewMCPServer(nil, "test"), api.NewCommitMCPServer(nil, "test"), nil)
	assert.Error(t, err)
	_, err = New(Config{MCPEnabled: true, OAuth: &cfg}, api.NewMCPServer(nil, "test"), nil, nil)
	assert.Error(t, err)
}
