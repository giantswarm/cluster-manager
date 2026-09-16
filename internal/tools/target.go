package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// kubeconfigSecretKey is the key CAPI writes the kubeconfig under.
const kubeconfigSecretKey = "value"

// TargetClientsFor returns the dynamic client a call uses to read a
// workload cluster: its apiserver and CA from the cluster's kubeconfig
// Secret, the identity the caller's own forwarded token — never the
// kubeconfig's credentials (the plan's caller-only rule). An error means the
// target cannot be read as the caller; detection then reports unknown.
type TargetClientsFor func(ctx context.Context, apiServer string, caBundle []byte) (dynamic.Interface, error)

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
}

// target resolves a Cluster to its detection target and backend target.
func (s *Service) target(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) target {
	t := target{
		Target:  detect.Target{Cluster: c.GetName(), Namespace: c.GetNamespace(), Installation: dyn},
		backend: compose.BackendTarget{Cluster: c.GetName(), Organization: organization(c), ServingNamespace: s.cfg.ServingNamespace, DiscoveryNamespace: c.GetNamespace()},
	}
	if s.ownCluster(c) {
		t.Reader = dyn
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
	reader, err := s.targets(ctx, apiServer, ca)
	if err != nil {
		t.Reason = fmt.Sprintf("cluster %s not readable as you through %s: %v", c.GetName(), apiServer, err)
		return t
	}
	t.Reader = reader
	return t
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
