package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The serving namespace outlives the slice (giantswarm/cluster-manager#154).
// The connectivity chart creates it with helm.sh/resource-policy: keep, so
// the model cache claim in it survives every teardown; once the slice is
// gone the namespace stays, labelled by a connectivity release that no
// longer exists, and reads to an operator — and to model-manager's commit
// resolution — as serving that is still there. A teardown of the slice
// therefore ends with the namespace: removed once its connectivity release
// is uninstalled and nothing is left in it; marked retired
// (RetiredAnnotation, naming what holds it) where something still is — the
// cache claims, a served object, a pod —, removing those being a person's
// explicit act (remove_model_cache for the claims). The slice composed again
// takes the mark off and Helm adopts the namespace as it stands.

// RetiredAnnotation marks a serving namespace the slice that created it has
// left: its value says when, why it stays and how it goes.
const RetiredAnnotation = "cluster-manager.giantswarm.io/retired"

// NamespaceGVR is the core namespaces API.
var NamespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// actionRetire marks the serving namespace retired instead of removing it.
const actionRetire = "retire"

// servingNamespaceOf reads the serving namespace on the target when it is
// the slice's — labelled by the cluster's connectivity release —; nil when
// it is absent or someone else's.
func (s *Service) servingNamespaceOf(ctx context.Context, t target) (*unstructured.Unstructured, error) {
	ns, err := t.Reader.Resource(NamespaceGVR).Get(ctx, s.cfg.ServingNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get Namespace %s on %s: %w", s.cfg.ServingNamespace, t.Cluster, err)
	}
	labels := ns.GetLabels()
	if labels[detect.LabelFluxReleaseName] != connectivityRelease(t.Cluster) || labels[detect.LabelFluxReleaseNamespace] != t.Namespace {
		return nil, nil
	}
	return ns, nil
}

// connectivityRelease is the slice's child release that creates the serving
// namespace.
func connectivityRelease(cluster string) string {
	return compose.SliceChildName(cluster, compose.ConnectivityComponent)
}

// retireServingNamespace is a slice teardown's last step: once the
// connectivity release is uninstalled — waited for within the budget, the
// re-run continues where it stands —, the slice's serving namespace on the
// target is removed when nothing is left in it, else marked retired naming
// what holds it. A namespace someone else made, or gone already, is left.
func (s *Service) retireServingNamespace(td *teardown, t target) error {
	if t.Reader == nil {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("whether the serving namespace %s remains on %s cannot be told (%s): the connectivity chart keeps it on uninstall — re-run disable_model_serving once the cluster is readable as you, and it is removed or marked retired", s.cfg.ServingNamespace, t.Cluster, t.Reason))
		return nil
	}
	ns, err := s.servingNamespaceOf(td.ctx, t)
	if err != nil || ns == nil {
		return err
	}
	act := ObjectAction{APIVersion: "v1", Kind: "Namespace", Name: ns.GetName()}
	if ts := ns.GetDeletionTimestamp(); ts != nil {
		act.Action = actionTerminating
		td.out.Objects = append(td.out.Objects, act)
		return nil
	}
	owner := connectivityRelease(t.Cluster)
	gone, err := td.waitGone(td.dyn.Resource(HelmReleaseGVR).Namespace(t.Namespace), owner)
	if err != nil {
		return err
	}
	holders, err := namespaceHolders(td.ctx, t.Reader, ns.GetName())
	if err != nil {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the serving namespace %s on %s is left as it is: what is left in it cannot be read as you (%v) — re-run once you may list its claims, pods and served models", ns.GetName(), t.Cluster, err))
		return nil
	}
	res := t.Reader.Resource(NamespaceGVR)
	if !gone && !td.dryRun {
		// Cut: the budget ran out while Helm uninstalled the release.
		act.Action = actionDelete
		if len(holders) > 0 {
			act.Action = actionRetire
		}
		td.record(act)
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the serving namespace %s on %s is pending: its connectivity release %s/%s is still being uninstalled — the re-run removes the namespace, or marks it retired where something is left in it", ns.GetName(), t.Cluster, t.Namespace, owner))
		return nil
	}
	if len(holders) == 0 {
		return td.delete(res, act)
	}
	reason := retiredReason(t.Cluster, holders)
	if ns.GetAnnotations()[RetiredAnnotation] != "" && strings.HasSuffix(ns.GetAnnotations()[RetiredAnnotation], reason) {
		act.Action = actionUnchanged
		td.out.Objects = append(td.out.Objects, act)
	} else {
		act.Action = actionRetire
		if td.fits() && !td.dryRun {
			value := time.Now().UTC().Format(time.RFC3339) + ": " + reason
			if err := annotate(td.ctx, res, "Namespace", ns.GetName(), RetiredAnnotation, &value); err != nil {
				return err
			}
		}
		td.record(act)
	}
	td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the serving namespace %s on %s is kept, marked retired (%s): %s", ns.GetName(), t.Cluster, RetiredAnnotation, reason))
	return nil
}

// retireLeftSlice is the slice's last steps where the cluster's slice
// release went before the call — the re-run of a teardown cut before them,
// of one from before they existed, or delete_node_pool once the pool is gone
// (giantswarm/cluster-manager#191, #203): the serving namespace the slice
// left is removed or marked retired (retireServingNamespace), and the
// Gateway API CRDs it composed — marked so before it went
// (markGatewayAPICRDs) — are removed (retireGatewayAPICRDs). While a slice
// release of that name stands, whoever's, both are its and nothing happens.
func (s *Service) retireLeftSlice(td *teardown, t target) error {
	gone, err := sliceReleaseGone(td.ctx, td.dyn, t.Namespace, t.Cluster)
	if err != nil || !gone {
		return err
	}
	if err := s.retireServingNamespace(td, t); err != nil {
		return err
	}
	marked, err := gatewayAPICRDsMarked(td.ctx, t)
	if err != nil || !marked {
		return err
	}
	return s.retireGatewayAPICRDs(td, t)
}

// sliceReleaseGone reports whether no slice release of the cluster's name
// exists on the installation, cluster-manager's or anyone's.
func sliceReleaseGone(ctx context.Context, dyn dynamic.Interface, ns, cluster string) (bool, error) {
	name := compose.SliceReleaseName(cluster)
	_, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get HelmRelease %s/%s: %w", ns, name, err)
	}
	return false, nil
}

// retireSliceLeftovers is a teardown call that finds the cluster's slice
// release gone already — disable_model_serving, or delete_node_pool of a
// pool removed since (giantswarm/cluster-manager#191, #203): the serving
// namespace and the Gateway API CRDs the slice left are retired
// (retireLeftSlice). The answer is nil when nothing of the slice's is left,
// or the cluster cannot be read as the caller: the tool's not found.
func (s *Service) retireSliceLeftovers(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, tool, mode string, dryRun bool, start time.Time) (*WriteResult, error) {
	t := s.target(ctx, dyn, c)
	if t.Reader == nil {
		return nil, nil
	}
	ns, err := s.servingNamespaceOf(ctx, t)
	if err != nil {
		return nil, err
	}
	marked, err := gatewayAPICRDsMarked(ctx, t)
	if err != nil {
		return nil, err
	}
	if (ns == nil || ns.GetDeletionTimestamp() != nil) && !marked {
		return nil, nil
	}
	out := &WriteResult{Cluster: c.GetName(), Namespace: c.GetNamespace(), Mode: mode, DryRun: dryRun, Objects: []ObjectAction{}}
	td := newTeardown(ctx, dyn, dryRun, out, s.budget(ctx, start))
	if err := s.retireLeftSlice(td, t); err != nil {
		return nil, err
	}
	td.finish()
	logApplied(ctx, tool, out, start)
	return out, nil
}

// retiredReason says why a retired serving namespace stays and how it goes.
func retiredReason(cluster string, holders []string) string {
	return fmt.Sprintf("model serving is off on %s, and the namespace still holds %s — remove_model_cache removes the model cache claims; once nothing is left, disable_model_serving removes the namespace, and enable_model_serving takes it back as it stands", cluster, strings.Join(holders, ", "))
}

// waitGone waits within the budget until the object of that name is gone;
// false when the teardown was cut first, or on a dry run while it stands.
func (td *teardown) waitGone(res dynamic.ResourceInterface, name string) (bool, error) {
	for {
		_, err := res.Get(td.ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("get %s: %w", name, err)
		}
		if td.dryRun || !td.fits() {
			return false, nil
		}
		if err := sleepWithin(td.ctx, pollInterval(time.Until(td.budget))); err != nil {
			return false, err
		}
	}
}

// namespaceHolders names what is left in a serving namespace that removing
// it would take along: claims (the model cache), served objects, and pods
// that still run.
func namespaceHolders(ctx context.Context, reader dynamic.Interface, namespace string) ([]string, error) {
	var holders []string
	claims, err := reader.Resource(detect.PersistentVolumeClaimGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list PersistentVolumeClaims in %s: %w", namespace, err)
	}
	for _, c := range claims.Items {
		holders = append(holders, "PersistentVolumeClaim "+c.GetName())
	}
	served, err := detect.ServedObjects(ctx, reader, namespace)
	if err != nil {
		return nil, err
	}
	for _, o := range served {
		holders = append(holders, o.GetKind()+" "+o.GetName())
	}
	pods, err := reader.Resource(detect.PodsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Pods in %s: %w", namespace, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.GetDeletionTimestamp() != nil {
			continue
		}
		switch nestedString(pod, "status", "phase") {
		case podSucceeded, podFailed:
			continue
		}
		holders = append(holders, "Pod "+pod.GetName())
	}
	return holders, nil
}

// annotate sets (value non-nil) or removes (nil) one annotation by a merge
// patch.
func annotate(ctx context.Context, res dynamic.ResourceInterface, kind, name, key string, value *string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}})
	if err != nil {
		return err
	}
	if _, err := res.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotate %s %s: %w", kind, name, err)
	}
	return nil
}

// reclaimServingNamespace is the counterpart before a slice is composed: a
// serving namespace being deleted is a refusal — the chart cannot install
// into it —, a retired one has the mark taken off, so Helm adopts it as it
// stands. Nothing happens on a target that cannot be read: the slice's
// composition has refused that case before.
func (s *Service) reclaimServingNamespace(ctx context.Context, t target, dryRun bool, out *WriteResult) error {
	if t.Reader == nil {
		return nil
	}
	ns, err := s.servingNamespaceOf(ctx, t)
	if err != nil || ns == nil {
		return err
	}
	if ts := ns.GetDeletionTimestamp(); ts != nil {
		return &ErrRefused{Reason: fmt.Sprintf("the serving namespace %s on %s is being deleted since %s (the serving slice that went removed it): the slice cannot be installed into it — re-run once it is gone", ns.GetName(), t.Cluster, ts.UTC().Format(time.RFC3339))}
	}
	if ns.GetAnnotations()[RetiredAnnotation] == "" {
		return nil
	}
	act := ObjectAction{APIVersion: "v1", Kind: "Namespace", Name: ns.GetName(), Action: actionUpdate}
	if dryRun {
		act.Action = "would-" + actionUpdate
	} else if err := annotate(ctx, t.Reader.Resource(NamespaceGVR), "Namespace", ns.GetName(), RetiredAnnotation, nil); err != nil {
		return err
	}
	out.Objects = append(out.Objects, act)
	return nil
}

// agentgatewayRelease is the slice's child release of agentgateway, whose
// controller watches the Gateway API kinds.
func agentgatewayRelease(cluster string) string {
	return compose.SliceChildName(cluster, "agentgateway")
}

// GatewayAPIComposedAnnotation marks the Gateway CRD on a target as composed
// by the slice release it names (<namespace>/<name>): the CRDs carry no
// label of the release whose hook Job applied them, and once the slice
// release is gone its values no longer say it composed them
// (giantswarm/cluster-manager#203).
const GatewayAPIComposedAnnotation = "cluster-manager.giantswarm.io/gateway-api-composed-by"

// composedBy is the value of GatewayAPIComposedAnnotation for the target's
// slice release.
func composedBy(t target) string {
	return t.Namespace + "/" + compose.SliceReleaseName(t.Cluster)
}

// markGatewayAPICRDs is the step before the slice release of a slice that
// composed the Gateway API CRDs goes: the Gateway CRD is annotated with the
// slice (GatewayAPIComposedAnnotation), so a re-run that finds the slice
// release gone still removes the CRDs (retireLeftSlice). Bookkeeping on an
// object the same teardown removes, so not answered as an object of its own;
// cut, the slice release's delete is pending with it and the re-run marks
// first.
func (s *Service) markGatewayAPICRDs(td *teardown, t target) error {
	if t.Reader == nil || td.dryRun {
		return nil
	}
	res := t.Reader.Resource(detect.CRDGVR)
	crd, err := res.Get(td.ctx, detect.GatewayAPICRD, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get CRD %s on %s: %w", detect.GatewayAPICRD, t.Cluster, err)
	}
	value := composedBy(t)
	if crd.GetAnnotations()[GatewayAPIComposedAnnotation] == value || !td.fits() {
		return nil
	}
	err = annotate(td.ctx, res, "CustomResourceDefinition", detect.GatewayAPICRD, GatewayAPIComposedAnnotation, &value)
	if apierrors.IsForbidden(err) {
		// The CRDs' delete is refused alike: retireGatewayAPICRDs says so.
		return nil
	}
	return err
}

// gatewayAPICRDsMarked reports whether the target's Gateway CRD is marked
// composed by the cluster's slice release (markGatewayAPICRDs).
func gatewayAPICRDsMarked(ctx context.Context, t target) (bool, error) {
	if t.Reader == nil {
		return false, nil
	}
	crd, err := t.Reader.Resource(detect.CRDGVR).Get(ctx, detect.GatewayAPICRD, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get CRD %s on %s: %w", detect.GatewayAPICRD, t.Cluster, err)
	}
	return crd.GetAnnotations()[GatewayAPIComposedAnnotation] == composedBy(t), nil
}

// gatewayAPICRDsInOrder is the bundle with the marked Gateway CRD last: a
// removal cut half-way leaves the mark, and the re-run removes the rest.
func gatewayAPICRDsInOrder() []string {
	names := make([]string, 0, len(compose.GatewayAPICRDs))
	for _, name := range compose.GatewayAPICRDs {
		if name != detect.GatewayAPICRD {
			names = append(names, name)
		}
	}
	return append(names, detect.GatewayAPICRD)
}

// retireGatewayAPICRDs is the teardown's step for the Gateway API CRDs a
// slice composed (giantswarm/cluster-manager#183): the gateway-api-crds chart
// applies them from a hook Job, so its uninstall leaves them. Once the
// connectivity release and agentgateway are uninstalled — their objects of
// those kinds gone with them, waited for within the budget; the CRDs pending
// when the budget runs out first, the re-run removing them (#203) — the CRDs
// are removed as the caller, unless objects of the kinds remain that none of
// the slice's releases rendered: removing the CRDs would take those along, so
// they stay, named.
func (s *Service) retireGatewayAPICRDs(td *teardown, t target) error {
	crds := strings.Join(compose.GatewayAPICRDs, " ")
	if t.Reader == nil {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the Gateway API CRDs the slice composed on %s are left (%s): the cluster cannot be read as you — remove them once nothing uses them (kubectl delete crd %s)", t.Cluster, t.Reason, crds))
		return nil
	}
	for _, release := range []string{connectivityRelease(t.Cluster), agentgatewayRelease(t.Cluster)} {
		gone, err := td.waitGone(td.dyn.Resource(HelmReleaseGVR).Namespace(t.Namespace), release)
		if err != nil {
			return err
		}
		if !gone && !td.dryRun {
			// Cut: the budget ran out while Helm uninstalled the release.
			for _, name := range gatewayAPICRDsInOrder() {
				td.record(ObjectAction{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: name, Action: actionDelete})
			}
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the Gateway API CRDs the slice composed on %s are pending: %s/%s is still being uninstalled — the re-run removes them once it is gone and nothing else uses them", t.Cluster, t.Namespace, release))
			return nil
		}
	}
	foreign, err := detect.ForeignGatewayAPIObjects(td.ctx, t.Reader, compose.GatewayAPICRDs, t.Namespace, t.Cluster)
	if err != nil {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the Gateway API CRDs the slice composed on %s are left: what uses them cannot be read as you (%v) — remove them once nothing does (kubectl delete crd %s)", t.Cluster, err, crds))
		return nil
	}
	if len(foreign) > 0 {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the Gateway API CRDs the slice composed on %s are kept: %s use them, and removing the CRDs would remove those too", t.Cluster, strings.Join(foreign, ", ")))
		return nil
	}
	res := t.Reader.Resource(detect.CRDGVR)
	for _, name := range gatewayAPICRDsInOrder() {
		err := td.delete(res, ObjectAction{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: name})
		if apierrors.IsForbidden(err) {
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the Gateway API CRDs the slice composed on %s are left: %v — remove them as someone allowed to (kubectl delete crd %s)", t.Cluster, err, crds))
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
