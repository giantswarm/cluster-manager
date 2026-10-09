package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// kubeconfigSecretKey is the key CAPI writes the kubeconfig under.
const kubeconfigSecretKey = "value"

// TargetClientsFor returns the clients a call uses to read a workload
// cluster: its apiserver and CA from the cluster's kubeconfig Secret, the
// identity the caller's own forwarded token — never the kubeconfig's
// credentials (the plan's caller-only rule). An error means the target cannot
// be read as the caller; detection then reports unknown.
type TargetClientsFor func(ctx context.Context, apiServer string, caBundle []byte) (Clients, error)

// target is a cluster as the detection and the backend registration reach
// it: the installation's own cluster is read through the installation's
// client and registered as the `local` target; a workload cluster through
// its own apiserver as the caller.
type target struct {
	detect.Target
	backend compose.BackendTarget
	// backendErr says why the backend cannot be registered (no kubeconfig
	// Secret, no cluster section in it).
	backendErr error
	// warmup is how long reaching the workload cluster took: its kubeconfig
	// Secret, the client and the first answer of its apiserver — connection,
	// TLS and the caller's token accepted. A write call's budget does not
	// count it (Service.budget), and its log line names it apart.
	warmup time.Duration
}

// target resolves a Cluster to its detection target and backend target. A
// workload cluster's client is warmed before the target is handed out: one
// version request, so the reads after it go over an established connection
// and the time a cold one takes is the target's warmup rather than the
// reads' (giantswarm/cluster-manager#207). The warm-up is not cut short: a
// connection abandoned half-way would leave the next call as cold; the
// request's deadline and client-go's dial and TLS timeouts bound it.
func (s *Service) target(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) target {
	start := time.Now()
	t := target{
		Target:  detect.Target{Cluster: c.GetName(), Namespace: c.GetNamespace(), SliceNamespace: compose.SliceWorkloadNamespace, Installation: dyn},
		backend: compose.BackendTarget{Cluster: c.GetName(), Organization: organization(c), ServingNamespace: s.cfg.ServingNamespace, DiscoveryNamespace: compose.SliceWorkloadNamespace},
	}
	if s.ownCluster(c) {
		t.Reader = dyn
		t.SliceNamespace = c.GetNamespace()
		t.backend.DiscoveryNamespace = c.GetNamespace()
		t.backend.OwnCluster = true
		return t
	}
	apiServer, ca, err := kubeconfigEndpoint(ctx, dyn, c.GetNamespace(), c.GetName())
	if err != nil {
		t.Reason = err.Error()
		t.backendErr = err
		return t
	}
	t.backend.APIServer, t.backend.CABundle = apiServer, string(ca)
	if s.targets == nil {
		t.Reason = "reading workload clusters is not configured on this server"
		return t
	}
	k, err := s.targets(ctx, apiServer, ca)
	if err == nil {
		err = warm(ctx, k.Discovery)
	}
	t.warmup = time.Since(start)
	if err != nil {
		t.Reason = fmt.Sprintf("cluster %s not readable as you through %s: %v", c.GetName(), apiServer, err)
		return t
	}
	t.Reader = k.Dynamic
	return t
}

// warm asks a workload cluster's apiserver for its version: the request
// every later read of the call finds its connection open from.
func warm(ctx context.Context, disc discovery.DiscoveryInterface) error {
	if _, err := discovery.ToDiscoveryInterfaceWithContext(disc).ServerVersionWithContext(ctx); err != nil {
		return fmt.Errorf("get version: %w", err)
	}
	return nil
}

// ownCluster reports whether c is the installation's own cluster.
func (s *Service) ownCluster(c *unstructured.Unstructured) bool {
	return s.cfg.Installation != "" && c.GetName() == s.cfg.Installation
}

// kubeconfigEndpoint reads the apiserver URL and CA of a workload cluster
// from the cluster section of its CAPI kubeconfig Secret. The user section
// — the cluster's admin credential — is never read.
func kubeconfigEndpoint(ctx context.Context, dyn dynamic.Interface, namespace, cluster string) (string, []byte, error) {
	name := compose.KubeconfigSecretName(cluster)
	secret, err := dyn.Resource(compose.SecretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil, fmt.Errorf("kubeconfig Secret %s/%s not found: the cluster's apiserver and CA come from it", namespace, name)
	}
	if err != nil {
		return "", nil, fmt.Errorf("get kubeconfig Secret %s/%s: %w", namespace, name, err)
	}
	encoded, _, _ := unstructured.NestedString(secret.Object, "data", kubeconfigSecretKey)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", nil, fmt.Errorf("kubeconfig Secret %s/%s key %s: %w", namespace, name, kubeconfigSecretKey, err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return "", nil, fmt.Errorf("kubeconfig Secret %s/%s: %w", namespace, name, err)
	}
	for _, cl := range cfg.Clusters {
		if cl.Server == "" {
			continue
		}
		if len(cl.CertificateAuthorityData) == 0 {
			return "", nil, fmt.Errorf("kubeconfig Secret %s/%s: cluster %s carries no certificate-authority-data", namespace, name, cl.Server)
		}
		return cl.Server, cl.CertificateAuthorityData, nil
	}
	return "", nil, errors.New("kubeconfig Secret " + namespace + "/" + name + ": no cluster section")
}
