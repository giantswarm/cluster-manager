package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The nodes guard's other half (giantswarm/cluster-manager#59): a served
// model whose predictor runs on no node. The guard used to judge from the
// pool's nodes alone — a pod with a GPU or a predictor's on one of them —,
// and a predictor Pending on no node held nothing: with the pool's only node
// idle, a delete removed the slice, its controller and the backend from under
// a model still loaded, and left its LLMInferenceService behind with a
// finalizer nothing clears. Every serving object model-manager manages in the
// serving namespace counts now, whatever its phase: one on a node of the
// pool is a busy node; one on no node is waiting for one, and the delete is
// refused naming it.

// waitingModel is a served model of model-manager's whose predictor runs on
// no node: Pending, waiting for a node of the pool — or without a pod yet,
// its controller still composing the workload.
type waitingModel struct {
	model detect.ServedModel
	// pods are the predictor's pods not scheduled on a node, by name; none
	// when the controller has not created one yet.
	pods []string
}

// String names the model with where its predictor stands:
// `LLMInferenceService model-serving/qwen3-8b (Qwen/Qwen3-8B): pod model-serving/qwen3-8b-kserve-…-xnkd2 Pending on no node`.
func (w waitingModel) String() string {
	if len(w.pods) == 0 {
		return w.model.String() + ": no predictor pod yet"
	}
	return fmt.Sprintf("%s: pod %s Pending on no node", w.model, strings.Join(w.pods, ", "))
}

// waitingModels lists, among the models served on the target, the ones
// model-manager manages in the serving namespace whose predictor runs on no
// node — read as the caller from the namespace's pods, once. A model with a
// pod on a node is served there (on a node of this pool it is a busy node
// the guard names first; on another pool's it is that pool's) and does not
// count.
func waitingModels(ctx context.Context, reader dynamic.Interface, namespace string, models []detect.ServedModel) ([]waitingModel, error) {
	var managed []detect.ServedModel
	for _, m := range models {
		if m.Managed(namespace) {
			managed = append(managed, m)
		}
	}
	if len(managed) == 0 {
		return nil, nil
	}
	pods, err := reader.Resource(detect.PodsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list the pods of %s: %w", namespace, err)
	}
	scheduled := map[string]bool{}
	pending := map[string][]string{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		kind, name, ok := detect.PredictorOf(pod)
		if !ok {
			continue
		}
		switch nestedString(pod, "status", "phase") {
		case "Succeeded", "Failed":
			continue
		}
		key := kind + "/" + name
		if nestedString(pod, "spec", "nodeName") != "" {
			scheduled[key] = true
			continue
		}
		pending[key] = append(pending[key], pod.GetNamespace()+"/"+pod.GetName())
	}
	var out []waitingModel
	for _, m := range managed {
		key := m.Kind + "/" + m.Name
		if scheduled[key] {
			continue
		}
		sort.Strings(pending[key])
		out = append(out, waitingModel{model: m, pods: pending[key]})
	}
	return out, nil
}

// waitingGuard is the nodes guard once no node of the pool is busy: refused
// while a model model-manager serves waits for a node — its predictor would
// be stranded, the slice and the backend gone from under it with the
// cluster's last pool —, and while whether one does cannot be told. The
// refusal names the waiting models and the idle nodes that go with the pool
// once they are unloaded.
func (s *Service) waitingGuard(ctx context.Context, t target, pool string, idle []*poolNode) error {
	const rerun = "unload them first (model-manager's unload_model) and re-run, or pass force to delete the pool regardless"
	models, err := detect.ServedModels(ctx, t.Reader)
	if err != nil {
		return &ErrRefused{Reason: fmt.Sprintf("node pool %s runs no busy node on %s, but whether a served model waits for one cannot be told (%v): a predictor Pending for a node of the pool would be stranded by the delete — re-run once you may list the cluster's inference services, or pass force to delete the pool regardless", pool, t.Cluster, err)}
	}
	waiting, err := waitingModels(ctx, t.Reader, s.cfg.ServingNamespace, models)
	if err != nil {
		return &ErrRefused{Reason: fmt.Sprintf("node pool %s runs no busy node on %s, but whether a served model waits for one cannot be told (%v): a predictor Pending for a node of the pool would be stranded by the delete — re-run once you may list the pods of %s, or pass force to delete the pool regardless", pool, t.Cluster, err, s.cfg.ServingNamespace)}
	}
	if len(waiting) == 0 {
		return nil
	}
	names := make([]string, 0, len(waiting))
	details := make([]string, 0, len(waiting))
	for _, w := range waiting {
		names = append(names, w.model.String())
		details = append(details, w.String())
	}
	reason := fmt.Sprintf("node pool %s runs no busy node on %s, but %d served model(s) wait for a node of the pool: %s — removing the pool strands them (with the cluster's last pool the serving slice, its controller and the backend go too, and the serving object is left behind with a finalizer nothing clears) — %s", pool, t.Cluster, len(waiting), strings.Join(details, "; "), rerun)
	if len(idle) > 0 {
		reason += fmt.Sprintf("; %d idle node(s) (%s) go with the pool once the models are unloaded", len(idle), strings.Join(nodeNames(idle), ", "))
	}
	all := make([]string, 0, len(models))
	for _, m := range models {
		all = append(all, m.String())
	}
	return &ErrRefused{
		Reason:  reason,
		Refused: &Refused{Nodes: []string{}, Idle: nodeNames(idle), Models: all, Unscheduled: names, Hint: refusedHint, ReadFrom: readFromCluster},
	}
}
