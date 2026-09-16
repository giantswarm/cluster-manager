package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/api"
	"github.com/giantswarm/cluster-manager/internal/kube"
	"github.com/giantswarm/cluster-manager/internal/server"
	"github.com/giantswarm/cluster-manager/internal/tools"
)

type serveOptions struct {
	listen string

	kubeconfig            string
	kubeContext           string
	inCluster             bool
	installation          string
	modelManagerNamespace string
	servingNamespace      string

	mcpEnabled bool
	mcpPath    string

	oauthEnabled                  bool
	oauthBaseURL                  string
	oauthProvider                 string
	dexIssuerURL                  string
	dexClientID                   string
	dexClientSecret               string
	dexCAFile                     string
	dexAllowPrivateIP             bool
	googleClientID                string
	googleClientSecret            string
	oauthTrustedAudiences         string
	ssoAllowPrivateIPs            bool
	allowPublicClientRegistration bool
	downstreamOAuth               bool
}

func newServeCmd() *cobra.Command {
	o := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server",
		Long: `Run the cluster-manager MCP server. Every flag can also be set through the
environment variable named next to it; flags win over the environment.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("CLUSTER_MANAGER_LISTEN", ":8080"), "Listen address (CLUSTER_MANAGER_LISTEN)")
	f.StringVar(&o.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "Kubeconfig path; empty uses the default loading rules or in-cluster auth (KUBECONFIG)")
	f.StringVar(&o.kubeContext, "kube-context", envOr("KUBE_CONTEXT", ""), "Kubeconfig context override (KUBE_CONTEXT)")
	f.BoolVar(&o.inCluster, "in-cluster", envBool("KUBERNETES_IN_CLUSTER", false), "Force in-cluster Kubernetes auth (KUBERNETES_IN_CLUSTER)")
	f.StringVar(&o.modelManagerNamespace, "model-manager-namespace", envOr("CLUSTER_MANAGER_MODEL_MANAGER_NAMESPACE", "agent-platform"), "Namespace model-manager runs in: create_node_pool registers the serving cluster's kserve backend there (CLUSTER_MANAGER_MODEL_MANAGER_NAMESPACE)")
	f.StringVar(&o.servingNamespace, "serving-namespace", envOr("CLUSTER_MANAGER_SERVING_NAMESPACE", "model-serving"), "Namespace on a serving cluster where InferenceServices go, named in the registered kserve backend (CLUSTER_MANAGER_SERVING_NAMESPACE)")
	f.StringVar(&o.installation, "installation", envOr("CLUSTER_MANAGER_INSTALLATION", ""), "Name of the installation: the Cluster of that name is reported as the installation's own cluster by list_clusters (CLUSTER_MANAGER_INSTALLATION)")
	f.BoolVar(&o.mcpEnabled, "mcp-enabled", envBool("CLUSTER_MANAGER_MCP_ENABLED", true), "Serve the MCP streamable-HTTP endpoint (CLUSTER_MANAGER_MCP_ENABLED)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("CLUSTER_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (CLUSTER_MANAGER_MCP_PATH)")
	f.BoolVar(&o.oauthEnabled, "enable-oauth", envBool("CLUSTER_MANAGER_OAUTH_ENABLED", false), "Require an OAuth 2.1 bearer token on the MCP endpoint, validated against the platform IdP (mcp-oauth); the caller's identity travels with every request (CLUSTER_MANAGER_OAUTH_ENABLED)")
	f.StringVar(&o.oauthBaseURL, "oauth-base-url", envOr("CLUSTER_MANAGER_OAUTH_BASE_URL", ""), "Public base URL of this server: the issuer of its OAuth metadata, https or loopback http (CLUSTER_MANAGER_OAUTH_BASE_URL)")
	f.StringVar(&o.oauthProvider, "oauth-provider", envOr("CLUSTER_MANAGER_OAUTH_PROVIDER", server.ProviderDex), "Identity provider: dex or google (CLUSTER_MANAGER_OAUTH_PROVIDER)")
	f.StringVar(&o.dexIssuerURL, "dex-issuer-url", envOr("DEX_ISSUER_URL", ""), "Dex issuer URL (DEX_ISSUER_URL)")
	f.StringVar(&o.dexClientID, "dex-client-id", envOr("DEX_CLIENT_ID", ""), "Dex client ID (DEX_CLIENT_ID)")
	f.StringVar(&o.dexClientSecret, "dex-client-secret", envOr("DEX_CLIENT_SECRET", ""), "Dex client secret (DEX_CLIENT_SECRET)")
	f.StringVar(&o.dexCAFile, "dex-ca-file", envOr("DEX_CA_FILE", ""), "PEM CA bundle of a Dex with a private certificate; verifies discovery, token and JWKS calls (DEX_CA_FILE)")
	f.BoolVar(&o.dexAllowPrivateIP, "allow-private-oauth-urls", envBool("CLUSTER_MANAGER_OAUTH_ALLOW_PRIVATE_URLS", false), "Let the Dex issuer resolve to a private or loopback address, an in-cluster Dex (CLUSTER_MANAGER_OAUTH_ALLOW_PRIVATE_URLS)")
	f.StringVar(&o.googleClientID, "google-client-id", envOr("GOOGLE_CLIENT_ID", ""), "Google OAuth client ID (GOOGLE_CLIENT_ID)")
	f.StringVar(&o.googleClientSecret, "google-client-secret", envOr("GOOGLE_CLIENT_SECRET", ""), "Google OAuth client secret (GOOGLE_CLIENT_SECRET)")
	f.StringVar(&o.oauthTrustedAudiences, "oauth-trusted-audiences", envOr("OAUTH_TRUSTED_AUDIENCES", ""), "Comma-separated OAuth client IDs whose IdP id_tokens are accepted as bearer tokens — the platform client muster forwards tokens for and the portal logs in with (OAUTH_TRUSTED_AUDIENCES)")
	f.BoolVar(&o.ssoAllowPrivateIPs, "sso-allow-private-ips", envBool("SSO_ALLOW_PRIVATE_IPS", false), "Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (SSO_ALLOW_PRIVATE_IPS)")
	f.BoolVar(&o.allowPublicClientRegistration, "allow-public-client-registration", envBool("CLUSTER_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION", false), "Accept unauthenticated dynamic client registration; labs only (CLUSTER_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION)")
	f.BoolVar(&o.downstreamOAuth, "downstream-oauth", envBool("CLUSTER_MANAGER_DOWNSTREAM_OAUTH", false), "Call the Kubernetes API as the caller, with the caller's IdP token, for everything a request does — the ServiceAccount holds no permissions (the chart renders none). Needs --enable-oauth and an apiserver that trusts the IdP (CLUSTER_MANAGER_DOWNSTREAM_OAUTH)")
	return cmd
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	if o.downstreamOAuth && !o.oauthEnabled {
		return fmt.Errorf("--downstream-oauth needs --enable-oauth: without OAuth there is no caller token to present to the Kubernetes API")
	}

	clients, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster, Logger: log})
	if err != nil {
		return fmt.Errorf("cluster-manager needs Kubernetes access: %w", err)
	}
	// Per-call clients: the caller's own when the request carries the
	// caller's token (downstream OAuth), the ServiceAccount's otherwise.
	svc := tools.New(
		func(ctx context.Context) tools.Clients {
			k := clients.For(ctx)
			return tools.Clients{Dynamic: k.Dynamic, Discovery: k.Discovery}
		},
		func(ctx context.Context, apiServer string, ca []byte) (dynamic.Interface, error) {
			target, err := clients.ForTarget(ctx, apiServer, ca)
			if err != nil {
				return nil, err
			}
			return target.Dynamic, nil
		},
		tools.Config{Installation: o.installation, ModelManagerNamespace: o.modelManagerNamespace, ServingNamespace: o.servingNamespace},
	)

	cfg := server.Config{Addr: o.listen, MCPEnabled: o.mcpEnabled, MCPPath: o.mcpPath}
	if o.oauthEnabled {
		cfg.OAuth = &server.OAuthConfig{
			BaseURL:                       o.oauthBaseURL,
			Provider:                      o.oauthProvider,
			DexIssuerURL:                  o.dexIssuerURL,
			DexClientID:                   o.dexClientID,
			DexClientSecret:               o.dexClientSecret,
			DexCAFile:                     o.dexCAFile,
			DexAllowPrivateIP:             o.dexAllowPrivateIP,
			GoogleClientID:                o.googleClientID,
			GoogleClientSecret:            o.googleClientSecret,
			TrustedAudiences:              splitList(o.oauthTrustedAudiences),
			SSOAllowPrivateIPs:            o.ssoAllowPrivateIPs,
			AllowPublicClientRegistration: o.allowPublicClientRegistration,
			DownstreamOAuth:               o.downstreamOAuth,
		}
	}
	srv, err := server.New(cfg, api.NewMCPServer(svc, version), log)
	if err != nil {
		return err
	}
	log.Info("cluster-manager starting", "version", version, "listen", o.listen, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled, "oauth", o.oauthEnabled, "downstreamOAuth", o.downstreamOAuth, "installation", o.installation)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
