package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// TestGitHubPinCarriesBothTokens is the App-pinned registration: the bearer
// is the person's App user token, verified with GET /user once per cache
// lifetime and kept for commit mode; the forwarded ID token in
// X-Muster-Id-Token is validated as a forwarded bearer is without the pin and
// becomes the caller and the Kubernetes token, so apply mode is unchanged.
func TestGitHubPinCarriesBothTokens(t *testing.T) {
	gh, calls := fakeGitHub(t, "ghu_person", "jane")
	o := newTestOAuthRuntime(OAuthConfig{DownstreamOAuth: true})
	guard, err := newGitHubGuard(GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-cluster-manager", APIURL: gh.URL}, o.log)
	require.NoError(t, err)

	const forwarded = "forwarded-id-token"
	// accepted stands for mcp-oauth's validation of the forwarded ID token.
	accepted := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if bearerToken(r) != forwarded {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(handler.ContextWithUserInfo(r.Context(), forwardedUser)))
		})
	}

	var seen struct {
		id    *identity.Identity
		token string
		gh    *identity.GitHub
	}
	h := guard.protect(accepted(o.attachIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, _ = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		seen.gh, _ = identity.GitHubFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))))
	call := func(bearer, idToken string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
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
	assert.Contains(t, rec.Body.String(), "core_auth_login server=cluster-manager")

	rec = call("", forwarded)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestGitHubPinValidation(t *testing.T) {
	assert.Error(t, GitHubPin{}.Validate())
	assert.Error(t, GitHubPin{AuthorizationServer: "github.com/apps/x"}.Validate())
	assert.NoError(t, GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-cluster-manager"}.Validate())
}
