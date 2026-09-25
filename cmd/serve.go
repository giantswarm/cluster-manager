package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/mcp-toolkit/tracing"
	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/api"
	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
	"github.com/giantswarm/cluster-manager/internal/kube"
	"github.com/giantswarm/cluster-manager/internal/registry"
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
	servingCacheClaim     string
	sliceChartVersion     string
	tenantServiceAccount  string
	certificateIssuer     string
	operatorDCGMExporter  bool
	applyBudget           time.Duration

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
	githubAuthorizationServer     string
	githubAPIURL                  string
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
	f.StringVar(&o.servingNamespace, "serving-namespace", envOr("CLUSTER_MANAGER_SERVING_NAMESPACE", "model-serving"), "Namespace on a serving cluster where LLMInferenceServices go, named in the registered kserve backend (CLUSTER_MANAGER_SERVING_NAMESPACE)")
	f.StringVar(&o.servingCacheClaim, "serving-cache-claim", envOr("CLUSTER_MANAGER_SERVING_CACHE_CLAIM", tools.DefaultCacheClaimName), "Base name of the model cache PersistentVolumeClaims in the serving namespace (the connectivity chart's default modelServing.cache.pvc.name; the claim of a zone is <name>-<zone>): create_node_pool composes the zone's claim into the pool's slice and pins the pool's nodes to the zone, list_clusters names every claim (CLUSTER_MANAGER_SERVING_CACHE_CLAIM)")
	f.StringVar(&o.sliceChartVersion, "slice-chart-version", envOr("CLUSTER_MANAGER_SLICE_CHART_VERSION", ""), "Pin the <cluster>-agent-platform slice release's agent-platform chart to this exact version; empty pins the version the installation's own platform release runs, at least "+compose.MinSliceChartVersion+" (CLUSTER_MANAGER_SLICE_CHART_VERSION)")
	f.StringVar(&o.tenantServiceAccount, "tenant-service-account", envOr("CLUSTER_MANAGER_TENANT_SERVICE_ACCOUNT", compose.DefaultTenantServiceAccount), "ServiceAccount in every org namespace the installation's Flux runs a composed release under when its objects live in that namespace (the pool release, the <cluster>-agent-platform slice release); the org's tenant ServiceAccount on Giant Swarm installations, empty renders none (CLUSTER_MANAGER_TENANT_SERVICE_ACCOUNT)")
	f.StringVar(&o.certificateIssuer, "models-certificate-issuer", envOr("CLUSTER_MANAGER_MODELS_CERTIFICATE_ISSUER", compose.DefaultCertificateIssuer), "cert-manager ClusterIssuer the <cluster>-agent-platform slice release asks for the models host's certificate when the platform's wildcard is not usable (a workload cluster; the own cluster when the platform's release names no gatewayApi.gateway.tls.secretName); empty composes none (CLUSTER_MANAGER_MODELS_CERTIFICATE_ISSUER)")
	f.BoolVar(&o.operatorDCGMExporter, "gpu-operator-dcgm-exporter", envBool("CLUSTER_MANAGER_GPU_OPERATOR_DCGM_EXPORTER", false), "Run NVIDIA's DCGM exporter on the GPU pools' nodes through the composed <cluster>-gpu-operator release (its dcgmExporter.enabled), for an installation whose observability scrapes it; off by default — nothing scrapes it out of the box, and on a fresh pool node it is an image pull and a pod initialising DCGM on the GPU while the device plugin brings nvidia.com/gpu up (CLUSTER_MANAGER_GPU_OPERATOR_DCGM_EXPORTER)")
	f.StringVar(&o.installation, "installation", envOr("CLUSTER_MANAGER_INSTALLATION", ""), "Name of the installation: the Cluster of that name is reported as the installation's own cluster by list_clusters (CLUSTER_MANAGER_INSTALLATION)")
	f.DurationVar(&o.applyBudget, "apply-budget", envDuration("CLUSTER_MANAGER_APPLY_BUDGET", tools.DefaultApplyBudget), "How long a write call (create_node_pool, enable_model_serving, delete_node_pool, disable_model_serving) may take before it stops writing and answers with what it did, the rest pending for the re-run: the aggregator's deadline for an upstream tool call less the answer's way back; a deadline the request carries wins when earlier (CLUSTER_MANAGER_APPLY_BUDGET)")
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
	f.StringVar(&o.githubAuthorizationServer, "github-authorization-server", envOr("CLUSTER_MANAGER_GITHUB_AUTHORIZATION_SERVER", ""), "Issuer identity of the GitHub App muster pins this server's registration to (https://github.com/apps/giantswarm-cluster-manager): the bearer of every call is then the person's App user token, verified with GET /user and used for commit mode's pull request, and the person's IdP ID token arrives in X-Muster-Id-Token (MCPServer auth.forwardIdentity). Empty: the bearer is the forwarded IdP ID token and commit mode is not offered. Needs --enable-oauth (CLUSTER_MANAGER_GITHUB_AUTHORIZATION_SERVER)")
	f.StringVar(&o.githubAPIURL, "github-api-url", envOr("CLUSTER_MANAGER_GITHUB_API_URL", server.DefaultGitHubAPIURL), "GitHub REST API base URL for GET /user and commit mode (CLUSTER_MANAGER_GITHUB_API_URL)")
	return cmd
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	if o.downstreamOAuth && !o.oauthEnabled {
		return fmt.Errorf("--downstream-oauth needs --enable-oauth: without OAuth there is no caller token to present to the Kubernetes API")
	}
	if o.githubAuthorizationServer != "" && !o.oauthEnabled {
		return fmt.Errorf("--github-authorization-server needs --enable-oauth: the forwarded IdP ID token is validated by the OAuth resource server")
	}

	// OTLP export when OTEL_EXPORTER_OTLP_ENDPOINT is set (the chart's
	// observability.otel); the W3C propagator either way.
	shutdownTracing, err := tracing.Init(ctx, tracing.WithServiceName("cluster-manager"), tracing.WithServiceVersion(version))
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			log.Warn("tracing shutdown", "error", err)
		}
	}()

	clients, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster, Logger: log})
	if err != nil {
		return fmt.Errorf("cluster-manager needs Kubernetes access: %w", err)
	}
	// The DNS-01 zone discovery is judged as cert-manager runs it: through
	// the pod's recursive nameservers.
	zones, err := detect.SystemResolver()
	if err != nil {
		return fmt.Errorf("cluster-manager needs a DNS resolver: %w", err)
	}
	// Per-call clients: the caller's own when the request carries the
	// caller's token (downstream OAuth), the ServiceAccount's otherwise.
	clientsFor := func(ctx context.Context) tools.Clients {
		k := clients.For(ctx)
		return tools.Clients{Dynamic: k.Dynamic, Discovery: k.Discovery}
	}
	targetsFor := func(ctx context.Context, apiServer string, ca []byte) (dynamic.Interface, error) {
		target, err := clients.ForTarget(ctx, apiServer, ca)
		if err != nil {
			return nil, err
		}
		return target.Dynamic, nil
	}
	toolsCfg := tools.Config{Installation: o.installation, ModelManagerNamespace: o.modelManagerNamespace, ServingNamespace: o.servingNamespace, CacheClaimName: o.servingCacheClaim, SliceChartVersion: o.sliceChartVersion, TenantServiceAccount: o.tenantServiceAccount, CertificateIssuer: o.certificateIssuer, OperatorDCGMExporter: o.operatorDCGMExporter, ApplyBudget: o.applyBudget}
	opts := []tools.Option{
		// The platform's charts from their registry, anonymously: what the
		// slice would install is read from there before it exists.
		tools.WithChartReader(&registry.Client{HTTP: &http.Client{Timeout: 20 * time.Second}}),
		tools.WithSOAQuerier(zones),
	}
	if o.githubAuthorizationServer != "" {
		// Commit mode: the pull request is opened as the person, with the
		// App user token the pinned registration carries.
		apiURL := o.githubAPIURL
		opts = append(opts, tools.WithGitHub(func(token string) (tools.GitHubRemote, error) {
			if apiURL == server.DefaultGitHubAPIURL {
				return commit.NewGitHub(token)
			}
			return commit.NewGitHub(token, commit.WithBaseURL(apiURL))
		}))
	}
	svc := tools.New(clientsFor, targetsFor, toolsCfg, opts...)

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
		if o.githubAuthorizationServer != "" {
			cfg.OAuth.GitHub = &server.GitHubPin{AuthorizationServer: o.githubAuthorizationServer, APIURL: o.githubAPIURL}
		}
	}
	srv, err := server.New(cfg, api.NewMCPServer(svc, version), log)
	if err != nil {
		return err
	}
	log.Info("cluster-manager starting", "version", version, "listen", o.listen, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled, "oauth", o.oauthEnabled, "downstreamOAuth", o.downstreamOAuth, "installation", o.installation, "commit", o.githubAuthorizationServer != "")

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

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
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
