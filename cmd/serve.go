package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/mcp-oauth/oauthconfig"
	oauthserver "github.com/giantswarm/mcp-oauth/server"
	"github.com/giantswarm/mcp-toolkit/metrics"
	"github.com/giantswarm/mcp-toolkit/tracing"
	"github.com/spf13/cobra"

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
	ssoAllowPrivateIPJWKSHosts    string
	allowPublicClientRegistration bool
	downstreamOAuth               bool
	githubAPIURL                  string
	githubExchangeEndpoint        string
	githubExchangeClient          string
	githubExchangeClientFile      string
	githubExchangeSecretFile      string
	githubExchangeAudience        string
	githubExchangeCAFile          string

	clusterOIDCClientID      string
	clusterOIDCUsernameClaim string
	clusterOIDCGroupsClaim   string
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
	f.StringVar(&o.sliceChartVersion, "slice-chart-version", envOr("CLUSTER_MANAGER_SLICE_CHART_VERSION", ""), "Pin the <cluster>-agent-platform slice release's agent-platform chart to this exact version; empty follows the chart from the version the installation's own platform release runs, at least "+compose.MinSliceChartVersion+" (CLUSTER_MANAGER_SLICE_CHART_VERSION)")
	f.StringVar(&o.tenantServiceAccount, "tenant-service-account", envOr("CLUSTER_MANAGER_TENANT_SERVICE_ACCOUNT", compose.DefaultTenantServiceAccount), "ServiceAccount in every org namespace the installation's Flux runs a composed release under when its objects live in that namespace (the pool release, the <cluster>-agent-platform slice release); the org's tenant ServiceAccount on Giant Swarm installations, empty renders none (CLUSTER_MANAGER_TENANT_SERVICE_ACCOUNT)")
	f.StringVar(&o.certificateIssuer, "models-certificate-issuer", envOr("CLUSTER_MANAGER_MODELS_CERTIFICATE_ISSUER", compose.DefaultCertificateIssuer), "cert-manager ClusterIssuer the <cluster>-agent-platform slice release asks for the models host's certificate when the platform's wildcard is not usable (a workload cluster; the own cluster when the platform's release names no gatewayApi.gateway.tls.secretName); empty composes none (CLUSTER_MANAGER_MODELS_CERTIFICATE_ISSUER)")
	f.BoolVar(&o.operatorDCGMExporter, "gpu-operator-dcgm-exporter", envBool("CLUSTER_MANAGER_GPU_OPERATOR_DCGM_EXPORTER", false), "Run NVIDIA's DCGM exporter on the GPU pools' nodes through the composed <cluster>-gpu-operator release (its dcgmExporter.enabled), for an installation whose observability scrapes it; off by default — nothing scrapes it out of the box, and on a fresh pool node it is an image pull and a pod initialising DCGM on the GPU while the device plugin brings nvidia.com/gpu up (CLUSTER_MANAGER_GPU_OPERATOR_DCGM_EXPORTER)")
	f.StringVar(&o.installation, "installation", envOr("CLUSTER_MANAGER_INSTALLATION", ""), "Name of the installation: the Cluster of that name is reported as the installation's own cluster by list_clusters (CLUSTER_MANAGER_INSTALLATION)")
	f.DurationVar(&o.applyBudget, "apply-budget", envDuration("CLUSTER_MANAGER_APPLY_BUDGET", tools.DefaultApplyBudget), "How long a write call (create_node_pool, enable_model_serving, delete_node_pool, disable_model_serving) may take before it stops writing and answers with what it did, the rest pending for the re-run: the aggregator's deadline for an upstream tool call less the answer's way back; a deadline the request carries wins when earlier (CLUSTER_MANAGER_APPLY_BUDGET)")
	f.StringVar(&o.clusterOIDCClientID, "cluster-oidc-client-id", envOr("CLUSTER_MANAGER_CLUSTER_OIDC_CLIENT_ID", ""), "Audience a cluster create_cluster makes accepts in the person's id_token: the cross-client audience muster requests at login, the one the installation's own apiserver trusts (dex-k8s-authenticator on Giant Swarm installations). With the Dex issuer (OAUTH_DEX_ISSUER_URL, its CA from OAUTH_DEX_CA_FILE), else the OAUTH_TRUSTED_ISSUERS entry marked openIDProvider (its caFile), it is composed into the cluster's global.controlPlane.oidc; empty composes none and the cluster trusts no identity provider (CLUSTER_MANAGER_CLUSTER_OIDC_CLIENT_ID)")
	f.StringVar(&o.clusterOIDCUsernameClaim, "cluster-oidc-username-claim", envOr("CLUSTER_MANAGER_CLUSTER_OIDC_USERNAME_CLAIM", "email"), "id_token claim a created cluster's apiserver takes as the username (CLUSTER_MANAGER_CLUSTER_OIDC_USERNAME_CLAIM)")
	f.StringVar(&o.clusterOIDCGroupsClaim, "cluster-oidc-groups-claim", envOr("CLUSTER_MANAGER_CLUSTER_OIDC_GROUPS_CLAIM", "groups"), "id_token claim a created cluster's apiserver takes as the groups (CLUSTER_MANAGER_CLUSTER_OIDC_GROUPS_CLAIM)")
	f.BoolVar(&o.mcpEnabled, "mcp-enabled", envBool("CLUSTER_MANAGER_MCP_ENABLED", true), "Serve the MCP streamable-HTTP endpoint (CLUSTER_MANAGER_MCP_ENABLED)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("CLUSTER_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (CLUSTER_MANAGER_MCP_PATH)")
	f.BoolVar(&o.oauthEnabled, "enable-oauth", envBool("CLUSTER_MANAGER_OAUTH_ENABLED", false), "Require an OAuth 2.1 bearer token on the MCP endpoint (mcp-oauth, configured by the OAUTH_* variables); the caller's identity travels with every request (CLUSTER_MANAGER_OAUTH_ENABLED)")
	f.StringVar(&o.oauthBaseURL, "oauth-base-url", envOr("CLUSTER_MANAGER_OAUTH_BASE_URL", ""), "Deprecated: set OAUTH_ISSUER, the public base URL of this server (CLUSTER_MANAGER_OAUTH_BASE_URL)")
	f.StringVar(&o.oauthProvider, "oauth-provider", envOr("CLUSTER_MANAGER_OAUTH_PROVIDER", ""), "Deprecated: set OAUTH_PROVIDER (dex, the default, or google) (CLUSTER_MANAGER_OAUTH_PROVIDER)")
	f.StringVar(&o.dexIssuerURL, "dex-issuer-url", envOr("DEX_ISSUER_URL", ""), "Deprecated: set OAUTH_DEX_ISSUER_URL (DEX_ISSUER_URL)")
	f.StringVar(&o.dexClientID, "dex-client-id", envOr("DEX_CLIENT_ID", ""), "Deprecated: set OAUTH_DEX_CLIENT_ID (DEX_CLIENT_ID)")
	f.StringVar(&o.dexClientSecret, "dex-client-secret", envOr("DEX_CLIENT_SECRET", ""), "Deprecated: set OAUTH_DEX_CLIENT_SECRET or OAUTH_DEX_CLIENT_SECRET_FILE (DEX_CLIENT_SECRET)")
	f.StringVar(&o.dexCAFile, "dex-ca-file", envOr("DEX_CA_FILE", ""), "Deprecated: set OAUTH_DEX_CA_FILE, the PEM CA bundle of a Dex with a private certificate (DEX_CA_FILE)")
	f.BoolVar(&o.dexAllowPrivateIP, "allow-private-oauth-urls", envBool("CLUSTER_MANAGER_OAUTH_ALLOW_PRIVATE_URLS", false), "Deprecated, no effect: the Dex client reaches private addresses and follows HTTP_PROXY, HTTPS_PROXY and NO_PROXY without it (CLUSTER_MANAGER_OAUTH_ALLOW_PRIVATE_URLS)")
	f.StringVar(&o.googleClientID, "google-client-id", envOr("GOOGLE_CLIENT_ID", ""), "Deprecated: set OAUTH_GOOGLE_CLIENT_ID (GOOGLE_CLIENT_ID)")
	f.StringVar(&o.googleClientSecret, "google-client-secret", envOr("GOOGLE_CLIENT_SECRET", ""), "Deprecated: set OAUTH_GOOGLE_CLIENT_SECRET or OAUTH_GOOGLE_CLIENT_SECRET_FILE (GOOGLE_CLIENT_SECRET)")
	f.StringVar(&o.oauthTrustedAudiences, "oauth-trusted-audiences", envOr("OAUTH_TRUSTED_AUDIENCES", ""), "Deprecated flag: set OAUTH_TRUSTED_AUDIENCES, the comma-separated OAuth client IDs whose IdP id_tokens are accepted as bearer tokens")
	f.BoolVar(&o.ssoAllowPrivateIPs, "sso-allow-private-ips", envBool("SSO_ALLOW_PRIVATE_IPS", false), "Deprecated, no effect: the issuer host's JWKS endpoint may resolve to a private address without it; list any other JWKS host in --sso-allow-private-ip-jwks-hosts (SSO_ALLOW_PRIVATE_IPS)")
	f.StringVar(&o.ssoAllowPrivateIPJWKSHosts, "sso-allow-private-ip-jwks-hosts", envOr("SSO_ALLOW_PRIVATE_IP_JWKS_HOSTS", ""), "Deprecated: set OAUTH_ALLOW_PRIVATE_IP_JWKS_HOSTS (SSO_ALLOW_PRIVATE_IP_JWKS_HOSTS)")
	f.BoolVar(&o.allowPublicClientRegistration, "allow-public-client-registration", envBool("CLUSTER_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION", false), "Deprecated: set OAUTH_ALLOW_PUBLIC_CLIENT_REGISTRATION (CLUSTER_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION)")
	f.BoolVar(&o.downstreamOAuth, "downstream-oauth", envBool("CLUSTER_MANAGER_DOWNSTREAM_OAUTH", false), "Call the Kubernetes API as the caller, with the caller's ID token, for everything a request does — the ServiceAccount holds no permissions (the chart renders none). Needs --enable-oauth and an apiserver that trusts the IdP (CLUSTER_MANAGER_DOWNSTREAM_OAUTH)")
	f.StringVar(&o.githubAPIURL, "github-api-url", envOr("CLUSTER_MANAGER_GITHUB_API_URL", server.DefaultGitHubAPIURL), "GitHub REST API base URL for GET /user on the fetched grant and commit mode's pull request (CLUSTER_MANAGER_GITHUB_API_URL)")
	f.StringVar(&o.githubExchangeEndpoint, "github-token-exchange-endpoint", envOr("CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_ENDPOINT", ""), "RFC 8693 endpoint (muster's broker) that releases the person's GitHub App user grant for commit mode, in place of the App-pinned registration (CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_ENDPOINT)")
	f.StringVar(&o.githubExchangeClient, "github-token-exchange-client-id", envOr("CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CLIENT_ID", ""), "Confidential token exchange client ID; wins over --github-token-exchange-client-id-file (CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CLIENT_ID)")
	f.StringVar(&o.githubExchangeClientFile, "github-token-exchange-client-id-file", envOr("CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CLIENT_ID_FILE", ""), "Mounted confidential client ID file, read at startup when --github-token-exchange-client-id is empty (CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CLIENT_ID_FILE)")
	f.StringVar(&o.githubExchangeSecretFile, "github-token-exchange-client-secret-file", envOr("CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CLIENT_SECRET_FILE", ""), "Mounted confidential client secret file (CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CLIENT_SECRET_FILE)")
	f.StringVar(&o.githubExchangeAudience, "github-token-exchange-audience", envOr("CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_AUDIENCE", ""), "RFC 8693 audience: the provider App whose grant the broker releases, named in grant_required answers (CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_AUDIENCE)")
	f.StringVar(&o.githubExchangeCAFile, "github-token-exchange-ca-file", envOr("CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CA_FILE", ""), "Additional CA bundle for token exchange (CLUSTER_MANAGER_GITHUB_TOKEN_EXCHANGE_CA_FILE)")
	return cmd
}

func warnDeprecatedOAuthOptions(o *serveOptions, log *slog.Logger) {
	if o.dexAllowPrivateIP {
		log.Warn("--allow-private-oauth-urls (CLUSTER_MANAGER_OAUTH_ALLOW_PRIVATE_URLS) has no effect and will be removed: the Dex client reaches private addresses and follows HTTP_PROXY, HTTPS_PROXY and NO_PROXY without it")
	}
	if o.ssoAllowPrivateIPs {
		log.Warn("--sso-allow-private-ips (SSO_ALLOW_PRIVATE_IPS) has no effect and will be removed: the issuer host's JWKS endpoint may resolve to a private address without it; list any other JWKS host in --sso-allow-private-ip-jwks-hosts")
	}
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	warnDeprecatedOAuthOptions(o, log)
	if o.downstreamOAuth && !o.oauthEnabled {
		return fmt.Errorf("--downstream-oauth needs --enable-oauth: without OAuth there is no caller token to present to the Kubernetes API")
	}
	var oauthSettings oauthconfig.Settings
	if o.oauthEnabled {
		var err error
		if oauthSettings, err = loadOAuthSettings(o, log); err != nil {
			return fmt.Errorf("oauth: %w", err)
		}
	}
	if err := validateGitHubExchange(o, oauthSettings); err != nil {
		return err
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
	shutdownMetrics, err := metrics.Init(ctx, metrics.WithServiceName("cluster-manager"), metrics.WithServiceVersion(version))
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdownMetrics(flushCtx); err != nil {
			log.Warn("flushing metrics", "error", err)
		}
	}()

	clients, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster})
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
	clientsFor := func(ctx context.Context) (tools.Clients, error) {
		caller, err := clients.For(ctx)
		if err != nil {
			return tools.Clients{}, err
		}
		return tools.Clients{Dynamic: caller.Dynamic, Discovery: caller.Discovery}, nil
	}
	targetsFor := func(ctx context.Context, apiServer string, ca []byte) (tools.Clients, error) {
		target, err := clients.ForTarget(ctx, apiServer, ca)
		if err != nil {
			return tools.Clients{}, err
		}
		return tools.Clients{Dynamic: target.Dynamic, Discovery: target.Discovery}, nil
	}
	toolsCfg := tools.Config{Installation: o.installation, ModelManagerNamespace: o.modelManagerNamespace, ServingNamespace: o.servingNamespace, CacheClaimName: o.servingCacheClaim, SliceChartVersion: o.sliceChartVersion, TenantServiceAccount: o.tenantServiceAccount, CertificateIssuer: o.certificateIssuer, OperatorDCGMExporter: o.operatorDCGMExporter, ApplyBudget: o.applyBudget}
	if toolsCfg.ClusterOIDC, err = o.clusterOIDC(oauthSettings, log); err != nil {
		return err
	}
	opts := []tools.Option{
		// The platform's charts from their registry, anonymously: what the
		// slice would install is read from there before it exists.
		tools.WithChartReader(&registry.Client{HTTP: &http.Client{Timeout: 20 * time.Second}}),
		tools.WithSOAQuerier(zones),
	}
	if o.githubExchangeEndpoint != "" {
		// Commit mode: the pull request is opened as the person, with the
		// grant fetched from muster's broker for the call.
		apiURL := o.githubAPIURL
		user, err := server.NewGitHubExchange(o.gitHubExchangeConfig(), log)
		if err != nil {
			return err
		}
		opts = append(opts, tools.WithGitHub(func(token string) (tools.GitHubRemote, error) {
			if apiURL == server.DefaultGitHubAPIURL {
				return commit.NewGitHub(token)
			}
			return commit.NewGitHub(token, commit.WithBaseURL(apiURL))
		}), tools.WithGitHubUser(user))
	}
	svc := tools.New(clientsFor, targetsFor, toolsCfg, opts...)

	cfg := server.Config{Addr: o.listen, MCPEnabled: o.mcpEnabled, MCPPath: o.mcpPath}
	if o.oauthEnabled {
		cfg.OAuth = &server.OAuthConfig{Settings: oauthSettings, DownstreamOAuth: o.downstreamOAuth}
	}
	srv, err := server.New(cfg, api.NewMCPServer(svc, version), log)
	if err != nil {
		return err
	}
	log.Info("cluster-manager starting", "version", version, "listen", o.listen, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled, "oauth", o.oauthEnabled, "downstreamOAuth", o.downstreamOAuth, "installation", o.installation, "commit", o.githubExchangeEndpoint != "")

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
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

// clusterOIDC is the identity provider a created cluster is composed to
// trust: the Dex this server signs people in with, else the trusted issuer
// marked as the OpenID Provider (its CA from its caFile), with the audience
// the installation's apiserver accepts. Nil without an audience or such a
// provider; the latter is logged, since the --cluster-oidc-* flags then have
// no effect.
func (o *serveOptions) clusterOIDC(settings oauthconfig.Settings, log *slog.Logger) (*compose.ClusterOIDC, error) {
	if !o.oauthEnabled || o.clusterOIDCClientID == "" {
		return nil, nil
	}
	var issuer, caFile string
	switch {
	case settings.Dex != nil && settings.Dex.IssuerURL != "":
		issuer, caFile = settings.Dex.IssuerURL, os.Getenv("OAUTH_DEX_CA_FILE")
	case settings.Google == nil:
		provider, present, err := singleOpenIDProvider(settings.TrustedIssuers)
		if err != nil {
			return nil, err
		}
		if !present {
			log.Warn("created clusters trust no identity provider: configure a Dex provider or one trusted issuer marked openIDProvider")
			return nil, nil
		}
		issuer = provider.Issuer
		if caFile, err = trustedIssuerCAFile(os.Getenv("OAUTH_TRUSTED_ISSUERS"), issuer); err != nil {
			return nil, err
		}
	default:
		log.Warn("created clusters trust no identity provider: --cluster-oidc-client-id is set, but there is neither a Dex provider (OAUTH_DEX_ISSUER_URL) nor a trusted issuer marked openIDProvider (OAUTH_TRUSTED_ISSUERS)")
		return nil, nil
	}
	out := &compose.ClusterOIDC{IssuerURL: issuer, ClientID: o.clusterOIDCClientID, UsernameClaim: o.clusterOIDCUsernameClaim, GroupsClaim: o.clusterOIDCGroupsClaim}
	if caFile != "" {
		ca, err := os.ReadFile(caFile) // #nosec G304 G703 -- operator-provided path
		if err != nil {
			return nil, fmt.Errorf("read the identity provider's CA for the created clusters' OIDC: %w", err)
		}
		out.CAPem = string(ca)
	}
	return out, nil
}

func singleOpenIDProvider(issuers []oauthserver.TrustedIssuer) (oauthserver.TrustedIssuer, bool, error) {
	var provider oauthserver.TrustedIssuer
	present := false
	for _, issuer := range issuers {
		if issuer.OpenIDProvider {
			if present {
				return oauthserver.TrustedIssuer{}, false, errors.New("OAUTH_TRUSTED_ISSUERS must mark exactly one openIDProvider to select the identity provider")
			}
			provider, present = issuer, true
		}
	}
	return provider, present, nil
}

// trustedIssuerCAFile is the caFile of the OAUTH_TRUSTED_ISSUERS entry for
// issuer: mcp-oauth keeps only the loaded pool, and a created cluster needs
// the PEM.
func trustedIssuerCAFile(raw, issuer string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var entries []struct {
		Issuer string `json:"issuer"`
		CAFile string `json:"caFile"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return "", fmt.Errorf("OAUTH_TRUSTED_ISSUERS: %w", err)
	}
	for _, entry := range entries {
		if entry.Issuer == issuer {
			return entry.CAFile, nil
		}
	}
	return "", nil
}

func (o *serveOptions) gitHubExchangeConfig() server.GitHubExchangeConfig {
	return server.GitHubExchangeConfig{TokenEndpoint: o.githubExchangeEndpoint, ClientID: o.githubExchangeClient, ClientIDFile: o.githubExchangeClientFile, ClientSecretFile: o.githubExchangeSecretFile, Audience: o.githubExchangeAudience, CAFile: o.githubExchangeCAFile, APIURL: o.githubAPIURL}
}

// The exchange uses the caller's provider ID token and downstream authorization.
func validateGitHubExchange(o *serveOptions, settings oauthconfig.Settings) error {
	if o.githubExchangeEndpoint == "" {
		if o.githubExchangeClient != "" || o.githubExchangeClientFile != "" || o.githubExchangeSecretFile != "" || o.githubExchangeAudience != "" || o.githubExchangeCAFile != "" {
			return errors.New("GitHub token exchange options require --github-token-exchange-endpoint")
		}
		return nil
	}
	hasProvider := settings.Dex != nil && settings.Dex.IssuerURL != ""
	if !hasProvider && settings.Google == nil {
		_, present, err := singleOpenIDProvider(settings.TrustedIssuers)
		if err != nil {
			return err
		}
		hasProvider = present
	}
	if !o.oauthEnabled || !o.downstreamOAuth || !hasProvider {
		return errors.New("GitHub token exchange requires OAuth, downstream OAuth and an OpenID Provider")
	}
	return o.gitHubExchangeConfig().Validate()
}
