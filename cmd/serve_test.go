package cmd

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
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
		"plain HTTP endpoint":      {func(o *serveOptions) { o.githubExchangeEndpoint = "http://muster.example/oauth/token" }, "HTTPS token endpoint"},
		"without audience":         {func(o *serveOptions) { o.githubExchangeAudience = "" }, "an audience"},
	} {
		t.Run(name, func(t *testing.T) {
			options := exchange()
			scenario.change(options)
			require.ErrorContains(t, validateGitHubExchange(options), scenario.want)
		})
	}
}

func TestWarnDeprecatedOAuthOptions(t *testing.T) {
	tests := []struct {
		name    string
		options serveOptions
		want    string
	}{
		{name: "allow-private-oauth-urls set", options: serveOptions{dexAllowPrivateIP: true}, want: "--allow-private-oauth-urls (CLUSTER_MANAGER_OAUTH_ALLOW_PRIVATE_URLS) has no effect"},
		{name: "sso-allow-private-ips set", options: serveOptions{ssoAllowPrivateIPs: true}, want: "--sso-allow-private-ips (SSO_ALLOW_PRIVATE_IPS) has no effect"},
		{name: "unset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			warnDeprecatedOAuthOptions(&tc.options, slog.New(slog.NewTextHandler(&output, nil)))
			if tc.want == "" {
				assert.Empty(t, output.String())
				return
			}
			assert.Contains(t, output.String(), tc.want)
		})
	}
}
