package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/server"
)

func TestValidateGitHubExchange(t *testing.T) {
	exchange := func() *serveOptions {
		return &serveOptions{ // #nosec G101 -- configuration fixture with a path, no secret value
			oauthEnabled: true, downstreamOAuth: true, oauthProvider: server.ProviderDex,
			githubExchangeEndpoint: "https://muster.example/oauth/token", githubExchangeClient: "cluster-manager",
			githubExchangeSecretFile: "/etc/cluster-manager/github-token-exchange/client-secret", githubExchangeAudience: "github-cluster-manager",
			githubAPIURL: server.DefaultGitHubAPIURL,
		}
	}
	require.NoError(t, validateGitHubExchange(&serveOptions{}), "no exchange configured")
	require.NoError(t, validateGitHubExchange(exchange()))

	for name, scenario := range map[string]struct {
		change func(*serveOptions)
		want   string
	}{
		"option without endpoint":  {func(o *serveOptions) { o.githubExchangeEndpoint = "" }, "require --github-token-exchange-endpoint"},
		"without OAuth":            {func(o *serveOptions) { o.oauthEnabled = false }, "requires Dex OAuth and downstream OAuth"},
		"without downstream OAuth": {func(o *serveOptions) { o.downstreamOAuth = false }, "requires Dex OAuth and downstream OAuth"},
		"with Google":              {func(o *serveOptions) { o.oauthProvider = server.ProviderGoogle }, "requires Dex OAuth and downstream OAuth"},
		"with the GitHub pin": {func(o *serveOptions) {
			o.githubAuthorizationServer = "https://github.com/apps/giantswarm-cluster-manager"
		}, "replaces the GitHub-pinned MCP registration"},
		"plain HTTP endpoint": {func(o *serveOptions) { o.githubExchangeEndpoint = "http://muster.example/oauth/token" }, "HTTPS token endpoint"},
		"without audience":    {func(o *serveOptions) { o.githubExchangeAudience = "" }, "an audience"},
	} {
		t.Run(name, func(t *testing.T) {
			options := exchange()
			scenario.change(options)
			require.ErrorContains(t, validateGitHubExchange(options), scenario.want)
		})
	}
}
