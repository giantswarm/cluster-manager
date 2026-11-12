package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/identity"
	"github.com/giantswarm/cluster-manager/internal/tools"
)

// JWTs with a placeholder signature: the server validated the bearer before
// the exchange reads it.
var (
	personToken    = unsignedJWT(`{"sub":"alice"}`)
	delegatedToken = unsignedJWT(`{"sub":"alice","act":{"sub":"agent"}}`)
	opaqueToken    = "not-a-jwt"
)

func unsignedJWT(claims string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"RS256"}`)) + "." + encode([]byte(claims)) + ".signature"
}

func TestGitHubExchange(t *testing.T) {
	for _, scenario := range []string{"success", "delegated actor", "missing identity token", "no grant", "broker error", "redirect", "invalid token type", "refresh token", "missing expiry", "GitHub refuses", "GitHub unavailable", "unparseable identity token"} {
		t.Run(scenario, func(t *testing.T) {
			secretFile := filepath.Join(t.TempDir(), "client-secret")
			require.NoError(t, os.WriteFile(secretFile, []byte("fixture-secret"), 0o600))
			exchanges, verifications := 0, 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					exchanges++
					require.Equal(t, http.MethodPost, r.Method)
					require.NoError(t, r.ParseForm())
					require.Equal(t, oidc.GrantTypeTokenExchange, r.Form.Get("grant_type"))
					require.Equal(t, personToken, r.Form.Get("subject_token"))
					require.Equal(t, oidc.TokenTypeIDToken, r.Form.Get("subject_token_type"))
					require.Equal(t, "manager-app", r.Form.Get("audience"))
					require.Empty(t, r.Form.Get("actor_token"))
					client, secret, ok := r.BasicAuth()
					require.True(t, ok)
					require.Equal(t, "manager-client", client)
					require.Equal(t, "fixture-secret", secret)
					switch scenario {
					case "no grant":
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprint(w, `{"error":"invalid_target","error_description":"the requested audience cannot be served"}`)
						return
					case "broker error":
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = fmt.Fprint(w, "do not expose verified-id-token fixture-secret")
						return
					case "redirect":
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(http.StatusTemporaryRedirect)
						return
					}
					result := oidc.TokenExchangeResponse{AccessToken: "fixture-user-grant", TokenType: "Bearer", IssuedTokenType: oidc.TokenTypeAccessToken, ExpiresIn: 300} // #nosec G101 -- synthetic grants served by the test broker
					if scenario == "invalid token type" {
						result.IssuedTokenType = oidc.TokenTypeIDToken
					}
					if scenario == "refresh token" {
						result.RefreshToken = "fixture-refresh"
					}
					if scenario == "missing expiry" {
						result.ExpiresIn = 0
					}
					require.NoError(t, json.NewEncoder(w).Encode(result)) // #nosec G117 -- the fixture is a token endpoint
					return
				}
				require.Equal(t, "/user", r.URL.Path, "credentials must never follow a redirect")
				verifications++
				require.Empty(t, r.Header.Get("X-Muster-Id-Token"))
				require.Equal(t, "Bearer fixture-user-grant", r.Header.Get("Authorization"))
				switch scenario {
				case "GitHub refuses":
					w.WriteHeader(http.StatusUnauthorized)
					return
				case "GitHub unavailable":
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = fmt.Fprint(w, `{"login":"alice"}`)
			})
			upstream := httptest.NewTLSServer(handler)
			defer upstream.Close()
			caFile := filepath.Join(t.TempDir(), "ca.crt")
			require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0o600))
			var logged bytes.Buffer
			configuration := GitHubExchangeConfig{TokenEndpoint: upstream.URL + "/token", ClientID: "manager-client", ClientSecretFile: secretFile, Audience: "manager-app", CAFile: caFile, APIURL: upstream.URL}
			ctx := identity.ContextWith(t.Context(), &identity.Identity{Subject: "alice"})
			switch scenario {
			case "delegated actor":
				ctx = identity.ContextWithToken(ctx, delegatedToken)
			case "unparseable identity token":
				ctx = identity.ContextWithToken(ctx, opaqueToken)
			case "missing identity token":
			default:
				ctx = identity.ContextWithToken(ctx, personToken)
			}
			resolve, err := NewGitHubExchange(configuration, slog.New(slog.NewJSONHandler(&logged, nil)))
			require.NoError(t, err)
			result, err := resolve(ctx)
			if scenario == "success" {
				require.NoError(t, err)
				require.Equal(t, &identity.GitHub{Login: "alice", Token: "fixture-user-grant"}, result) // #nosec G101 -- synthetic broker response
				_, err = resolve(ctx)
				require.NoError(t, err)
				require.Equal(t, 2, exchanges)
				require.Equal(t, 2, verifications, "user grants are not cached")
				return
			}
			require.Error(t, err)
			require.Nil(t, result)
			for _, value := range []string{personToken, "fixture-secret", "fixture-user-grant", "fixture-refresh"} {
				require.NotContains(t, err.Error(), value)
			}
			grant, isGrant := errors.AsType[*tools.ErrGrantRequired](err)
			require.Equal(t, scenario == "no grant" || scenario == "GitHub refuses", isGrant, "%v", err)
			if isGrant {
				require.Equal(t, "manager-app", grant.App)
			}
			switch scenario {
			case "missing identity token", "delegated actor", "unparseable identity token":
				require.Zero(t, exchanges)
				_, refused := errors.AsType[*tools.ErrRefused](err)
				require.True(t, refused)
			}
			if scenario == "delegated actor" || scenario == "unparseable identity token" {
				require.ErrorContains(t, err, "forbidden: commit mode opens the pull request as you, and only you, not an agent acting for you")
			}
			if scenario == "no grant" {
				var record map[string]any
				require.NoError(t, json.Unmarshal(logged.Bytes(), &record))
				require.Equal(t, "WARN", record["level"])
				require.Equal(t, "GitHub token exchange refused", record["msg"])
				require.Equal(t, float64(http.StatusBadRequest), record["status"])
				require.Equal(t, "invalid_target", record["error"])
				require.Equal(t, "the requested audience cannot be served", record["errorDescription"])
			}
			require.NotContains(t, logged.String(), "fixture-secret")
			require.NotContains(t, logged.String(), personToken)
		})
	}
}

func TestGitHubExchangeConfiguration(t *testing.T) {
	valid := GitHubExchangeConfig{TokenEndpoint: "https://broker.example.com/oauth/token", ClientID: "manager", ClientSecretFile: "/mounted/client-secret", Audience: "manager-app"} // #nosec G101 -- configuration fixture with a path, no secret value
	require.NoError(t, valid.Validate())
	for _, endpoint := range []string{"http://broker.example.com/token", "https://user:password@broker.example.com/token", "https://broker.example.com/token?credential=value", "https://broker.example.com/token#fragment", "/token"} {
		configuration := valid
		configuration.TokenEndpoint = endpoint
		require.Error(t, configuration.Validate())
	}
	configuration := valid
	configuration.ClientSecretFile = ""
	require.Error(t, configuration.Validate())
	configuration = valid
	configuration.Audience = ""
	require.Error(t, configuration.Validate())
	configuration = valid
	configuration.ClientID = ""
	require.Error(t, configuration.Validate())
	configuration.ClientIDFile = "/mounted/client-id"
	require.NoError(t, configuration.Validate())
}

func TestGitHubExchangeClientID(t *testing.T) {
	directory := t.TempDir()
	clientIDFile := filepath.Join(directory, "client-id")
	require.NoError(t, os.WriteFile(clientIDFile, []byte("file-client\n"), 0o600))
	absent := filepath.Join(directory, "absent")

	for _, scenario := range []struct {
		name, flag, file, want string
	}{
		{name: "flag wins over file", flag: "flag-client", file: clientIDFile, want: "flag-client"},
		{name: "flag with the file key absent", flag: "flag-client", file: absent, want: "flag-client"},
		{name: "file", file: clientIDFile, want: "file-client"},
		{name: "neither", file: absent},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := GitHubExchangeConfig{ClientID: scenario.flag, ClientIDFile: scenario.file}.resolveClientID()
			if scenario.want == "" {
				require.ErrorContains(t, err, "client ID")
				return
			}
			require.NoError(t, err)
			require.Equal(t, scenario.want, got)
		})
	}

	_, err := NewGitHubExchange(GitHubExchangeConfig{TokenEndpoint: "https://broker.example.com/oauth/token", ClientIDFile: absent, ClientSecretFile: filepath.Join(directory, "client-secret"), Audience: "manager-app"}, slog.New(slog.DiscardHandler)) // #nosec G101 -- configuration fixture with paths, no secret value
	require.ErrorContains(t, err, "client ID", "startup fails without a client ID")
}

// TestGitHubExchangeTrustsOnlyItsCA: without the broker's CA the exchange
// never reaches it, and the answer is no grant_required.
func TestGitHubExchangeTrustsOnlyItsCA(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("fixture-secret"), 0o600))
	reached := false
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer upstream.Close()
	resolve, err := NewGitHubExchange(GitHubExchangeConfig{TokenEndpoint: upstream.URL + "/token", ClientID: "manager-client", ClientSecretFile: secretFile, Audience: "manager-app", APIURL: upstream.URL}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	ctx := identity.ContextWithToken(identity.ContextWith(t.Context(), &identity.Identity{Subject: "alice"}), personToken)
	_, err = resolve(ctx)
	require.ErrorContains(t, err, "GitHub authorization service is unavailable")
	_, isGrant := errors.AsType[*tools.ErrGrantRequired](err)
	require.False(t, isGrant)
	require.False(t, reached)
}
