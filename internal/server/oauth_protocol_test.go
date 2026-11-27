package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

func TestResourceServerOnlyMCPProtocol(t *testing.T) {
	provider := newPrivateIdP(t, "")
	trusted, err := json.Marshal([]map[string]any{{
		"issuer": provider.issuer, "jwksURI": provider.issuer + "/keys",
		"audiences": []string{"platform-client"}, "openIDProvider": true,
		"acceptedTypHeaders": []string{"JWT"}, "caFile": provider.caFile,
		"allowPrivateIPJWKSHosts": []string{privateIssuerHost},
	}})
	require.NoError(t, err)
	settings := oauthSettings(t, map[string]string{
		"OAUTH_ISSUER":                      "https://manager.example.test",
		"OAUTH_ENABLE_AUTHORIZATION_SERVER": "false",
		"OAUTH_AUTHORIZATION_SERVERS":       provider.issuer,
		"OAUTH_TRUSTED_ISSUERS":             string(trusted),
	})
	runtime, err := newOAuth(OAuthConfig{Settings: settings, DownstreamOAuth: true}, "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { runtime.shutdown(context.WithoutCancel(t.Context())) })
	protocol := mcpserver.NewStreamableHTTPServer(mcpserver.NewMCPServer("fixture", "test", mcpserver.WithToolCapabilities(false)), mcpserver.WithStateLess(true), mcpserver.WithDisableStreaming(true))
	endpoint := httptest.NewServer(runtime.protect(protocol))
	t.Cleanup(endpoint.Close)
	for _, scenario := range []struct {
		name, token string
		status      int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"platform audience", provider.idToken(t, []string{"platform-client"}), http.StatusOK},
		{"other audience", provider.idToken(t, []string{"manager-client"}), http.StatusUnauthorized},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"fixture","version":"test"},"io.modelcontextprotocol/clientCapabilities":{}}}}`))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("MCP-Protocol-Version", "2026-07-28")
			request.Header.Set("Mcp-Method", "tools/list")
			if scenario.token != "" {
				request.Header.Set("Authorization", "Bearer "+scenario.token)
			}
			response, err := endpoint.Client().Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			answer, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, scenario.status, response.StatusCode, string(answer))
			if scenario.status == http.StatusOK {
				var result struct {
					Result struct {
						Tools []any `json:"tools"`
					} `json:"result"`
					Error any `json:"error"`
				}
				require.NoError(t, json.Unmarshal(answer, &result))
				require.Nil(t, result.Error)
				require.NotNil(t, result.Result.Tools)
			}
		})
	}
}
