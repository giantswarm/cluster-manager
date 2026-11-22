// Package kube builds the Kubernetes clients cluster-manager uses for agent
// wiring and the kserve driver: in-cluster when running as a pod, kubeconfig
// (+ context) otherwise. With downstream OAuth on, a request that carries the
// caller's IdP token gets clients that present that token instead — the
// caller's RBAC governs what the request may do, and the ServiceAccount holds
// no permissions of its own (the chart renders no RBAC for it).
package kube

import (
	"context"
	"fmt"
	"os"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	toolkitk8s "github.com/giantswarm/mcp-toolkit/k8s"

	"github.com/giantswarm/cluster-manager/internal/identity"
)

// Factory selects explicitly configured application clients or caller clients.
type Factory struct {
	*toolkitk8s.Clients
	users     *toolkitk8s.UserClients
	userAgent string
}

// Config selects how to reach the API server.
type Config struct {
	// Kubeconfig is an explicit kubeconfig path; empty uses the default
	// loading rules ($KUBECONFIG, ~/.kube/config).
	Kubeconfig string
	// Context overrides the kubeconfig's current context.
	Context string
	// InCluster forces in-cluster auth. When false and no kubeconfig is
	// found, in-cluster auth is still tried if the pod environment is present.
	InCluster bool
}

// New builds the clients.
func New(cfg Config) (*Factory, error) {
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}
	restCfg.UserAgent = "cluster-manager"
	return fromRESTConfig(restCfg)
}

// One tool call is a burst of requests — create_node_pool reads some forty
// objects and writes eight — and client-go throttles a client whose config
// leaves QPS and Burst zero to 5 requests a second after the first ten, which
// alone held that call at the aggregator's deadline
// (giantswarm/cluster-manager#34). The limits below let a call's reads go at
// the apiserver's pace; the apiserver's own priority and fairness bounds them.
const (
	clientQPS   = 100
	clientBurst = 200
)

func fromRESTConfig(restCfg *rest.Config) (*Factory, error) {
	if restCfg.QPS == 0 {
		restCfg.QPS = clientQPS
	}
	if restCfg.Burst == 0 {
		restCfg.Burst = clientBurst
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	users, err := toolkitk8s.NewUserClients(restCfg, nil)
	if err != nil {
		return nil, err
	}
	return &Factory{Clients: &toolkitk8s.Clients{Dynamic: dyn, Discovery: disc, Clientset: cs}, users: users, userAgent: restCfg.UserAgent}, nil
}

// ForTarget returns clients for another cluster's apiserver — a workload
// cluster, reached at apiServer with its CA — presenting the caller's
// forwarded token and nothing else: the kubeconfig Secret's credentials are
// never used, so the caller's RBAC on the target governs (the target trusts
// the installation's identity provider). Without a caller token there is no
// identity to present and the target is not read.
func (c *Factory) ForTarget(ctx context.Context, apiServer string, ca []byte) (*toolkitk8s.Clients, error) {
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("no caller token: a workload cluster is read as the caller only")
	}
	configuration := &rest.Config{Host: apiServer, UserAgent: c.userAgent, TLSClientConfig: rest.TLSClientConfig{CAData: ca}, QPS: clientQPS, Burst: clientBurst}
	target, err := toolkitk8s.NewUserClients(configuration, nil)
	if err != nil {
		return nil, err
	}
	return target.ForToken(token)
}

// For selects caller clients when a token is present, including after expiry.
// Calls without a token use the explicitly configured base clients.
func (c *Factory) For(ctx context.Context) (*toolkitk8s.Clients, error) {
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return c.Clients, nil
	}
	clients, err := c.users.ForToken(token)
	if err != nil {
		return nil, fmt.Errorf("caller-token Kubernetes clients: %w", err)
	}
	return clients, nil
}

func restConfig(cfg Config) (*rest.Config, error) {
	if cfg.InCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return c, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err == nil {
		return c, nil
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if ic, icErr := rest.InClusterConfig(); icErr == nil {
			return ic, nil
		}
	}
	return nil, fmt.Errorf("kubeconfig: %w", err)
}
