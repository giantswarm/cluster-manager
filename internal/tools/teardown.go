package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The serving slice's teardown (giantswarm/cluster-manager#28). The slice
// release's kserve-runtime-configs child installs KServe's well-known
// LLMInferenceServiceConfigs into the release namespace on the target, and
// the llm-d controller (the kserve-llmisvc-resources child) puts
// detect.LLMISVCConfigFinalizer on each. Removing the slice release at once
// uninstalls both children in the same instant: the configs are deleted, the
// controller that would clear their finalizer is gone, and they sit
// terminating until the next slice installed there adopts and loses them —
// its first LLMInferenceService then fails with ConfigNotFound. So the slice
// goes in order: the configs' release first, then the configs themselves are
// seen gone — waited for while the controller runs, removed by cluster-manager
// where nothing else will (the controller gone, or force) —, then the rest.
const (
	// runtimeConfigsRelease is the slice's child HelmRelease of the
	// well-known configs, named after its component in the meta chart.
	runtimeConfigsRelease = "kserve-runtime-configs"
	// DefaultConfigTeardownTimeout bounds the wait for the controller: the
	// installation's helm-controller has to uninstall the child release and
	// the controller to clear ten finalizers — seconds, ordinarily.
	DefaultConfigTeardownTimeout = 2 * time.Minute
)

// servingTeardown removes the slice's serving objects that must go before the
// slice release: the kserve-runtime-configs child release, then every
// LLMInferenceServiceConfig of the release namespace on the target — waited
// for while the llm-d controller runs to clear them, removed with their
// finalizer where nothing will (the controller gone, force, or the wait
// over). Each object goes into out; dry-run records what would happen. Without
// force a target that cannot be read as the caller is a refusal: whether the
// configs are gone cannot be told.
func (s *Service) servingTeardown(ctx context.Context, dyn dynamic.Interface, t target, force, dryRun bool, out *WriteResult) error {
	ns, cluster := t.Namespace, t.Cluster
	if t.Reader == nil && !force {
		return &ErrRefused{Reason: fmt.Sprintf("cannot tell whether the LLMInferenceServiceConfigs of %s on %s are gone (%s): the serving slice is removed in order so none is left terminating — make the cluster readable as you and re-run, or pass force to remove the slice regardless", ns, cluster, t.Reason)}
	}
	if err := deleteChildRelease(ctx, dyn, ns, compose.SliceReleaseName(cluster), runtimeConfigsRelease, dryRun, out); err != nil {
		return err
	}
	if t.Reader == nil {
		out.Warnings = append(out.Warnings, fmt.Sprintf("whether LLMInferenceServiceConfigs remain in %s on %s cannot be told (%s): one left terminating with %s uncleared breaks the next serving slice installed there — check the namespace, or heal it by re-running create_node_pool once the cluster is readable as you", ns, cluster, t.Reason, detect.LLMISVCConfigFinalizer))
		return nil
	}
	reason := ""
	switch {
	case force:
		reason = "removed with force"
	case !detect.LLMISVCControllerRuns(ctx, t.Reader):
		reason = "no llmisvc controller runs on " + cluster + " to clear the finalizer"
	case dryRun:
		// The controller's path: the configs go with their release and
		// nothing of cluster-manager's touches them.
		return nil
	default:
		timeout := s.cfg.ConfigTeardownTimeout
		if timeout == 0 {
			timeout = DefaultConfigTeardownTimeout
		}
		left, err := awaitConfigsGone(ctx, t.Reader, ns, timeout)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		reason = fmt.Sprintf("still present %s after the %s release was removed", timeout, runtimeConfigsRelease)
	}
	configs, err := detect.Configs(ctx, t.Reader, ns)
	if err != nil {
		return err
	}
	return purgeConfigs(ctx, t.Reader, configs, cluster, reason, dryRun, out)
}

// healStrandedConfigs is the counterpart before a slice is composed: the
// LLMInferenceServiceConfigs terminating in the release namespace with no
// llm-d controller to clear them are removed, finalizer and all, so the
// slice's release creates them afresh instead of adopting and losing them;
// with a controller running they are its (models still reference them) and
// are named as a warning only. Nothing happens on a target that cannot be
// read: the slice's composition has refused that case before.
func healStrandedConfigs(ctx context.Context, t target, dryRun bool, out *WriteResult) error {
	if t.Reader == nil {
		return nil
	}
	configs, err := detect.Configs(ctx, t.Reader, t.Namespace)
	if err != nil {
		return err
	}
	stranded := detect.Terminating(configs)
	if len(stranded) == 0 {
		return nil
	}
	if detect.LLMISVCControllerRuns(ctx, t.Reader) {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d LLMInferenceServiceConfig(s) in %s on %s are terminating while an llmisvc controller runs (%s): models still referencing them hold them — unload those models, or wait for the controller to clear them; a slice release installed over a terminating config adopts and loses it", len(stranded), t.Namespace, t.Cluster, strings.Join(detect.Names(stranded), ", ")))
		return nil
	}
	return purgeConfigs(ctx, t.Reader, stranded, t.Cluster, "left terminating by a serving layer that went, no llmisvc controller to clear the finalizer; removed so the slice's release creates them afresh", dryRun, out)
}

// deleteChildRelease deletes one child HelmRelease of a slice release — an
// object Helm rendered, owned by the release through Flux's labels, never by
// cluster-manager's own —; not found, or someone else's, is left alone.
func deleteChildRelease(ctx context.Context, dyn dynamic.Interface, ns, release, child string, dryRun bool, out *WriteResult) error {
	res := dyn.Resource(HelmReleaseGVR).Namespace(ns)
	hr, err := res.Get(ctx, child, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get HelmRelease %s/%s: %w", ns, child, err)
	}
	labels := hr.GetLabels()
	if labels[detect.LabelFluxReleaseName] != release || labels[detect.LabelFluxReleaseNamespace] != ns {
		return nil
	}
	act := ObjectAction{APIVersion: hr.GetAPIVersion(), Kind: hr.GetKind(), Name: child, Namespace: ns, Action: "delete"}
	if dryRun {
		act.Action = "would-delete"
	} else if err := res.Delete(ctx, child, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete HelmRelease %s/%s: %w", ns, child, err)
	}
	out.Objects = append(out.Objects, act)
	return nil
}

// awaitConfigsGone polls until no LLMInferenceServiceConfig is left in the
// namespace or timeout passes, and names the ones left.
func awaitConfigsGone(ctx context.Context, reader dynamic.Interface, ns string, timeout time.Duration) ([]string, error) {
	var left []string
	err := wait.PollUntilContextTimeout(ctx, pollInterval(timeout), timeout, true, func(ctx context.Context) (bool, error) {
		configs, err := detect.Configs(ctx, reader, ns)
		if err != nil {
			return false, err
		}
		left = detect.Names(configs)
		return len(left) == 0, nil
	})
	if err != nil && !wait.Interrupted(err) {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return left, nil
}

// pollInterval is a twentieth of the wait, between 10ms and 2s.
func pollInterval(timeout time.Duration) time.Duration {
	return max(10*time.Millisecond, min(2*time.Second, timeout/20))
}

// purgeConfigs removes the configs from the target: deleted, then their
// llmisvc finalizer taken off — a terminating object goes the moment its last
// finalizer does, and a controller cannot put one back on an object that is
// gone. Every config goes into out, and one warning says why cluster-manager
// did what the controller does.
func purgeConfigs(ctx context.Context, reader dynamic.Interface, configs []unstructured.Unstructured, cluster, reason string, dryRun bool, out *WriteResult) error {
	if len(configs) == 0 {
		return nil
	}
	ns := configs[0].GetNamespace()
	for i := range configs {
		c := &configs[i]
		act := ObjectAction{APIVersion: c.GetAPIVersion(), Kind: c.GetKind(), Name: c.GetName(), Namespace: c.GetNamespace(), Action: "delete"}
		if dryRun {
			act.Action = "would-delete"
		} else if err := purgeConfig(ctx, reader, c.GetNamespace(), c.GetName()); err != nil {
			return err
		}
		out.Objects = append(out.Objects, act)
	}
	verb := "removed"
	if dryRun {
		verb = "would be removed"
	}
	out.Warnings = append(out.Warnings, fmt.Sprintf("%d LLMInferenceServiceConfig(s) in %s on %s %s by cluster-manager with %s taken off (%s): %s", len(configs), ns, cluster, verb, detect.LLMISVCConfigFinalizer, reason, strings.Join(detect.Names(configs), ", ")))
	return nil
}

// purgeConfig deletes one config and takes its llmisvc finalizer off,
// retrying the update on a conflict with the controller; gone already is
// fine.
func purgeConfig(ctx context.Context, reader dynamic.Interface, ns, name string) error {
	res := reader.Resource(detect.LLMISVCConfigGVR).Namespace(ns)
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete LLMInferenceServiceConfig %s/%s: %w", ns, name, err)
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		finalizers := withoutFinalizer(c.GetFinalizers(), detect.LLMISVCConfigFinalizer)
		if len(finalizers) == len(c.GetFinalizers()) {
			return nil
		}
		c.SetFinalizers(finalizers)
		_, err = res.Update(ctx, c, metav1.UpdateOptions{FieldManager: compose.ManagedBy})
		return err
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("clear %s of LLMInferenceServiceConfig %s/%s: %w", detect.LLMISVCConfigFinalizer, ns, name, err)
	}
	return nil
}

// withoutFinalizer is finalizers without name.
func withoutFinalizer(finalizers []string, name string) []string {
	out := make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if f != name {
			out = append(out, f)
		}
	}
	return out
}
