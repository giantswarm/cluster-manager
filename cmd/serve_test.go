package cmd

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/mcp-oauth/oauthconfig"
	"github.com/giantswarm/mcp-oauth/providers/dex"
	"github.com/giantswarm/mcp-oauth/providers/google"
	oauthserver "github.com/giantswarm/mcp-oauth/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/server"
)

func TestValidateGitHubExchange(t *testing.T) {
	exchange := func() *serveOptions {
		return &serveOptions{ // #nosec G101 -- configuration fixture with a path, no secret value
			oauthEnabled: true, downstreamOAuth: true,
			githubExchangeEndpoint: "https://muster.example/oauth/token", githubExchangeClient: "cluster-manager",
			githubExchangeSecretFile: "/etc/cluster-manager/github-token-exchange/client-secret", githubExchangeAudience: "github-cluster-manager",
			githubAPIURL: server.DefaultGitHubAPIURL,
		}
	}
	require.NoError(t, validateGitHubExchange(&serveOptions{}, true), "no exchange configured")
	require.NoError(t, validateGitHubExchange(exchange(), true))
	require.ErrorContains(t, validateGitHubExchange(exchange(), false), "requires Dex OAuth and downstream OAuth", "the subject token must be a Dex token")

	for name, scenario := range map[string]struct {
		change func(*serveOptions)
		want   string
	}{
		"option without endpoint":  {func(o *serveOptions) { o.githubExchangeEndpoint = "" }, "require --github-token-exchange-endpoint"},
		"without OAuth":            {func(o *serveOptions) { o.oauthEnabled = false }, "requires Dex OAuth and downstream OAuth"},
		"without downstream OAuth": {func(o *serveOptions) { o.downstreamOAuth = false }, "requires Dex OAuth and downstream OAuth"},
		"plain HTTP endpoint":      {func(o *serveOptions) { o.githubExchangeEndpoint = "http://muster.example/oauth/token" }, "HTTPS token endpoint"},
		"without audience":         {func(o *serveOptions) { o.githubExchangeAudience = "" }, "an audience"},
	} {
		t.Run(name, func(t *testing.T) {
			options := exchange()
			scenario.change(options)
			require.ErrorContains(t, validateGitHubExchange(options, true), scenario.want)
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

func TestClusterOIDC(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caFile, []byte("dex-ca-pem"), 0o600))
	t.Setenv("OAUTH_DEX_CA_FILE", caFile)
	t.Setenv("OAUTH_TRUSTED_ISSUERS", `[{"issuer":"https://other.example.com","caFile":"/nowhere"},{"issuer":"https://idp.example.com","caFile":"`+caFile+`","openIDProvider":true}]`)
	options := &serveOptions{oauthEnabled: true, clusterOIDCClientID: "dex-k8s-authenticator", clusterOIDCUsernameClaim: "email", clusterOIDCGroupsClaim: "groups"}
	want := func(issuer string) *compose.ClusterOIDC {
		return &compose.ClusterOIDC{IssuerURL: issuer, ClientID: "dex-k8s-authenticator", UsernameClaim: "email", GroupsClaim: "groups", CAPem: "dex-ca-pem"}
	}
	var output bytes.Buffer
	log := slog.New(slog.NewTextHandler(&output, nil))

	got, err := options.clusterOIDC(oauthconfig.Settings{Dex: &dex.Config{IssuerURL: "https://dex.example.com"}}, log)
	require.NoError(t, err)
	require.Equal(t, want("https://dex.example.com"), got, "the Dex provider people sign in with")

	openIDProvider := []oauthserver.TrustedIssuer{{Issuer: "https://other.example.com"}, {Issuer: "https://idp.example.com", OpenIDProvider: true}}
	got, err = options.clusterOIDC(oauthconfig.Settings{TrustedIssuers: openIDProvider}, log)
	require.NoError(t, err)
	require.Equal(t, want("https://idp.example.com"), got, "without Dex settings: the trusted issuer marked openIDProvider, with its caFile")
	require.Empty(t, output.String())

	got, err = options.clusterOIDC(oauthconfig.Settings{TrustedIssuers: openIDProvider[:1]}, log)
	require.NoError(t, err)
	require.Nil(t, got, "no OpenID Provider: created clusters trust none")
	require.Contains(t, output.String(), "created clusters trust no identity provider")

	output.Reset()
	got, err = options.clusterOIDC(oauthconfig.Settings{Google: &google.Config{}}, log)
	require.NoError(t, err)
	require.Nil(t, got, "a Google provider is not composed into created clusters")
	require.Contains(t, output.String(), "created clusters trust no identity provider")

	output.Reset()
	withoutAudience := *options
	withoutAudience.clusterOIDCClientID = ""
	got, err = withoutAudience.clusterOIDC(oauthconfig.Settings{Dex: &dex.Config{IssuerURL: "https://dex.example.com"}}, log)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Empty(t, output.String(), "nothing to warn about without the flags")
}
