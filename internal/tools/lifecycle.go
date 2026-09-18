package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// Phase is where a pool stands in its life, as list_node_pools reports it.
type Phase string

const (
	// PhaseCreating: the pool release is not Ready yet, or its MachinePool
	// not created or not ready.
	PhaseCreating Phase = "creating"
	// PhaseReady: release Ready, MachinePool ready, every node counted —
	// readyReplicas may be 0 at scale-to-zero.
	PhaseReady Phase = "ready"
	// PhaseScaling: NodeClaims launching or terminating, or nodes
	// registering.
	PhaseScaling Phase = "scaling"
	// PhaseRemoving: the pool release or its MachinePool carries a
	// deletionTimestamp, or a teardown re-run is pending.
	PhaseRemoving Phase = "removing"
	// PhaseFailed: the pool release reports Ready=False; the release step
	// carries the reason.
	PhaseFailed Phase = "failed"
)

// The states of a Step — one vocabulary across the managers.
const (
	StepPending    = "pending"
	StepInProgress = "inProgress"
	StepDone       = "done"
	StepFailed     = "failed"
)

// The steps of a pool's life, in order.
const (
	StepRelease     = "release"
	StepMachinePool = "machinePool"
	StepNodes       = "nodes"
	// StepPrewarm is the placeholder of a pool created with prewarm
	// (giantswarm/cluster-manager#48): the chart's one-shot Job holding one
	// GPU in the release namespace until the first workload preempts it.
	// Listed after the nodes for a pool whose release carries
	// pool.prewarm.enabled, never for another; it says where the first node
	// stands before any model is served and never decides the phase.
	StepPrewarm = "prewarm"
)

// The placeholder as the gpu-node-pool chart renders it: the Job named after
// the release, its pod labelled with the release's selector labels and the
// pool's identity.
const (
	prewarmJobSuffix     = "-prewarm"
	labelReleaseInstance = "app.kubernetes.io/instance"
	// jobReasonDeadline is the Job controller's reason when
	// activeDeadlineSeconds passed with the Job still active: no node came.
	jobReasonDeadline = "DeadlineExceeded"
)

// Step is one step of a pool's life: the pool release Ready, the MachinePool
// ready, the nodes (NodeClaims launching, ready, terminating), the prewarm
// placeholder where the pool has one.
type Step struct {
	Name string `json:"name"`
	// State is pending, inProgress, done or failed.
	State string `json:"state"`
	// Since is when the step entered its state (RFC3339): the condition's
	// lastTransitionTime, else the object's creation; empty while pending.
	Since string `json:"since,omitempty"`
	// FinishedAt is Since of a done step.
	FinishedAt string `json:"finishedAt,omitempty"`
	Message    string `json:"message,omitempty"`
}

// actionTerminating marks a pending object already deleted, finalizing.
const actionTerminating = "terminating"

// poolState is everything list_node_pools reads about one pool, concurrently.
type poolState struct {
	mp      *unstructured.Unstructured
	release *unstructured.Unstructured
	// infra is the pool's infrastructure object (KarpenterMachinePool or
	// AWSMachinePool), nil when unreadable.
	infra *unstructured.Unstructured
	// live is the pool's nodes as the cluster shows them — NodeClaims, Nodes
	// and what holds each; nil when the cluster, or neither of the two APIs,
	// is readable.
	live *poolLive
	// pending are the teardown's targets still present, in teardown order.
	pending []ObjectAction
	// sourceGone: the pool release exists but its OCIRepository does not —
	// a partial teardown removed it, the re-run finishes.
	sourceGone bool
	// prewarm is the placeholder of a pool whose release carries
	// pool.prewarm.enabled; nil for a pool without the option.
	prewarm *prewarmState
}

// prewarmState is the placeholder's Job and pod as read from the release
// namespace; the Job is nil when it is gone.
type prewarmState struct {
	namespace, name string
	job             *unstructured.Unstructured
	pods            []unstructured.Unstructured
}

// poolPrewarm reads whether a pool release asks for the placeholder: the
// chart's pool.prewarm.enabled, as compose.Pool writes it.
func poolPrewarm(hr *unstructured.Unstructured) bool {
	enabled, _, _ := unstructured.NestedBool(hr.Object, "spec", "values", "pool", "prewarm", "enabled")
	return enabled
}

// prewarmPodSelector selects the placeholder's pod by the labels the chart
// puts on it: the release's selector labels and the pool's identity.
func prewarmPodSelector(release string) string {
	return compose.LabelChartName + "=" + compose.PoolChart + "," + labelReleaseInstance + "=" + release + "," + compose.LabelMachinePool + "=" + release
}

// readPoolState reads a pool's objects concurrently: the owning release, the
// infrastructure, the NodeClaims on the cluster and the teardown's targets.
// A failed read leaves its part empty; the pool is still listed.
func (s *Service) readPoolState(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, t target, mp, release *unstructured.Unstructured, pool string) *poolState {
	r := &poolState{mp: mp, release: release}
	g, gctx := errgroup.WithContext(ctx)
	if mp != nil {
		g.Go(func() error {
			if ref := nestedRef(mp, "spec", "template", "spec", "infrastructureRef"); ref != nil {
				r.infra, _ = getRef(gctx, dyn, ref, mp.GetNamespace())
			}
			return nil
		})
	}
	if release == nil && mp != nil {
		if owner := ownerRelease(mp); owner != nil {
			g.Go(func() error {
				hr, err := dyn.Resource(HelmReleaseGVR).Namespace(owner.Namespace).Get(gctx, owner.Name, metav1.GetOptions{})
				if err == nil {
					r.release = hr
				}
				return nil
			})
		}
	}
	if t.Reader != nil {
		g.Go(func() error {
			if live := readPoolLive(gctx, t.Reader, pool); live.readable() {
				r.live = live
			}
			return nil
		})
	}
	if release != nil && compose.OwnedBy(release) {
		g.Go(func() error {
			targets, _, _, _, err := s.removalTargets(gctx, dyn, c, poolShortName(c.GetName(), pool))
			if err != nil {
				return nil //nolint:nilerr // the pool is listed without its pending objects
			}
			r.pending, r.sourceGone = pendingRemovals(gctx, dyn, targets, pool)
			return nil
		})
	}
	if release != nil && poolPrewarm(release) {
		p := &prewarmState{namespace: release.GetNamespace(), name: release.GetName() + prewarmJobSuffix}
		r.prewarm = p
		g.Go(func() error {
			job, err := dyn.Resource(JobGVR).Namespace(p.namespace).Get(gctx, p.name, metav1.GetOptions{})
			if err == nil {
				p.job = job
			}
			return nil
		})
		g.Go(func() error {
			pods, err := dyn.Resource(detect.PodsGVR).Namespace(p.namespace).List(gctx, metav1.ListOptions{LabelSelector: prewarmPodSelector(release.GetName())})
			if err == nil {
				p.pods = pods.Items
			}
			return nil
		})
	}
	_ = g.Wait()
	return r
}

// poolShortName is the pool's name as create_node_pool took it: the release
// name without the cluster's prefix.
func poolShortName(cluster, release string) string {
	return strings.TrimPrefix(release, cluster+"-")
}

// pendingRemovals reads the teardown's targets concurrently: the ones still
// present, terminating where deleted, in teardown order — and whether the
// pool's own OCIRepository is gone while its release stands.
func pendingRemovals(ctx context.Context, dyn dynamic.Interface, targets []objectRef, pool string) ([]ObjectAction, bool) {
	objs := make([]*unstructured.Unstructured, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	for i, target := range targets {
		g.Go(func() error {
			obj, err := dyn.Resource(target.gvr).Namespace(target.ns).Get(gctx, target.name, metav1.GetOptions{})
			if err == nil {
				objs[i] = obj
			}
			return nil
		})
	}
	_ = g.Wait()
	var (
		out        []ObjectAction
		sourceGone bool
	)
	for i, obj := range objs {
		if obj == nil {
			if targets[i].gvr == compose.OCIRepositoryGVR && targets[i].name == pool {
				sourceGone = true
			}
			continue
		}
		act := ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace(), Action: actionPending}
		if obj.GetDeletionTimestamp() != nil {
			act.Action = actionTerminating
		}
		out = append(out, act)
	}
	return out, sourceGone
}

// lifecycle derives a pool's phase and steps from its reads.
func lifecycle(r *poolState) (Phase, []Step, bool, []ObjectAction) {
	steps := []Step{}
	removing := r.sourceGone
	for _, p := range r.pending {
		if p.Action == actionTerminating {
			removing = true
		}
	}
	if r.release != nil {
		steps = append(steps, releaseStep(r.release))
		removing = removing || r.release.GetDeletionTimestamp() != nil
	}
	steps = append(steps, machinePoolStep(r.mp))
	if r.mp != nil && r.mp.GetDeletionTimestamp() != nil {
		removing = true
	}
	steps = append(steps, nodesStep(r.mp, r.infra, r.live, steps[len(steps)-1].State == StepDone))
	phase := poolPhase(steps)
	if r.prewarm != nil {
		steps = append(steps, prewarmStep(r.prewarm))
	}
	if removing {
		return PhaseRemoving, steps, true, r.pending
	}
	return phase, steps, false, nil
}

// poolPhase derives the phase from the release, MachinePool and nodes steps:
// failed when one failed, ready when every one is done, scaling while the
// nodes move, creating otherwise. The prewarm step is not among them: a
// placeholder pending, holding a node or ended says nothing about the pool.
func poolPhase(steps []Step) Phase {
	phase := PhaseReady
	for _, st := range steps {
		switch {
		case st.State == StepFailed:
			phase = PhaseFailed
		case st.State != StepDone && phase == PhaseReady:
			phase = PhaseCreating
			if st.Name == StepNodes && st.State == StepInProgress {
				phase = PhaseScaling
			}
		}
	}
	return phase
}

// prewarmStep words the placeholder's state (giantswarm/cluster-manager#48).
// The Job's terminal condition decides where it has one: Complete — the hold
// ended without a workload, done; Failed for DeadlineExceeded — no node came
// within the hold plus ten minutes, failed; Failed otherwise — the pod ended
// before its hold, preempted by the first workload (backoffLimit 0: it is not
// replaced), done. While the Job is active its pod says: Pending while the
// first node launches, Running while it holds the node, terminating while
// the first workload takes it. A Job that is gone — its TTL removed it ten
// minutes after it ended, or prewarm was set on an existing pool and the
// chart creates the Job on install only — is done, saying so.
func prewarmStep(p *prewarmState) Step {
	st := Step{Name: StepPrewarm, State: StepDone}
	job := p.namespace + "/" + p.name
	if p.job == nil {
		st.Message = "no placeholder Job " + job + ": removed ten minutes after it ended, or prewarm was set on an existing pool (the Job is created with the release's install only)"
		return st
	}
	st.Since = detect.Timestamp(p.job.GetCreationTimestamp().Time)
	if cond, found := detect.ConditionOf(p.job, "Complete"); found && cond.Status == "True" {
		st.FinishedAt = latest(cond.LastTransitionTime, nestedString(p.job, "status", "completionTime"))
		st.Message = "finished: the hold ended without a workload; Karpenter consolidates the empty node"
		return st
	}
	if cond, found := detect.ConditionOf(p.job, "Failed"); found && cond.Status == "True" {
		st.FinishedAt = cond.LastTransitionTime
		detail := strings.TrimSpace(cond.Reason + " " + strings.Join(strings.Fields(cond.Message), " "))
		if cond.Reason == jobReasonDeadline {
			st.State = StepFailed
			st.Message = fmt.Sprintf("no node came within the placeholder's deadline of %d s (%s): check the pool's NodeClaims and Karpenter's log", nestedInt(p.job, "spec", "activeDeadlineSeconds"), detail)
			return st
		}
		st.Message = "preempted: the first workload took the placeholder's node before its hold ended (" + detail + ")"
		return st
	}
	st.State = StepInProgress
	pod := newestPod(p.pods)
	if pod == nil {
		st.Message = "placeholder Job " + job + " created, its pod not yet"
		return st
	}
	node := nestedString(pod, "spec", "nodeName")
	switch phase := nestedString(pod, "status", "phase"); {
	case pod.GetDeletionTimestamp() != nil:
		st.Since = detect.Timestamp(pod.GetDeletionTimestamp().Time)
		st.Message = "preempted: the placeholder's pod is terminating, the first workload takes its node " + node
	case phase == "Running":
		st.Since = latest(st.Since, nestedString(pod, "status", "startTime"))
		st.Message = "holding node " + node + " until the first workload preempts the placeholder or its hold ends"
	case phase == "Pending":
		st.Since = latest(st.Since, detect.Timestamp(pod.GetCreationTimestamp().Time))
		st.Message = "pending: the placeholder waits for the pool's first node, launched by Karpenter for its GPU"
		if cond, found := detect.ConditionOf(pod, "PodScheduled"); found && cond.Status != "True" && cond.Message != "" {
			st.Message += " (" + strings.Join(strings.Fields(cond.Message), " ") + ")"
		}
	default:
		st.Message = "placeholder pod " + pod.GetName() + " " + phase + "; the Job has not concluded yet"
	}
	return st
}

// newestPod is the latest-created of the placeholder's pods (one, with
// backoffLimit 0; a second only while the first is terminating), nil for
// none.
func newestPod(pods []unstructured.Unstructured) *unstructured.Unstructured {
	var newest *unstructured.Unstructured
	for i := range pods {
		pod := &pods[i]
		if newest == nil || pod.GetCreationTimestamp().After(newest.GetCreationTimestamp().Time) {
			newest = pod
		}
	}
	return newest
}

// releaseStep reads the pool release's Ready condition: True is done, False
// failed with the reason, Unknown (Flux reconciling) or none yet in progress
// since the release was created.
func releaseStep(hr *unstructured.Unstructured) Step {
	st := Step{Name: StepRelease, State: StepInProgress, Since: detect.Timestamp(hr.GetCreationTimestamp().Time)}
	cond, found := detect.ReadyCondition(hr)
	if !found {
		st.Message = "HelmRelease " + hr.GetNamespace() + "/" + hr.GetName() + " reports no Ready condition yet"
		return st
	}
	if cond.LastTransitionTime != "" {
		st.Since = cond.LastTransitionTime
	}
	st.Message = strings.Join(strings.Fields(cond.Message), " ")
	switch cond.Status {
	case "True":
		st.State, st.FinishedAt = StepDone, st.Since
	case "False":
		st.State = StepFailed
		if cond.Reason != "" {
			st.Message = cond.Reason + ": " + st.Message
		}
	}
	return st
}

// machinePoolStep reads the MachinePool's Ready condition, else its
// infrastructureReady flag; pending while helm-controller has not created
// it.
func machinePoolStep(mp *unstructured.Unstructured) Step {
	if mp == nil {
		return Step{Name: StepMachinePool, State: StepPending, Message: "MachinePool not created yet"}
	}
	st := Step{Name: StepMachinePool, State: StepInProgress, Since: detect.Timestamp(mp.GetCreationTimestamp().Time)}
	cond, found := detect.ReadyCondition(mp)
	switch {
	case found && cond.LastTransitionTime != "":
		st.Since = cond.LastTransitionTime
	}
	ready, _, _ := unstructured.NestedBool(mp.Object, "status", "infrastructureReady")
	if (found && cond.Status == "True") || (!found && ready) {
		st.State, st.FinishedAt = StepDone, st.Since
		return st
	}
	if found {
		st.Message = strings.TrimSpace(cond.Reason + " " + strings.Join(strings.Fields(cond.Message), " "))
	} else {
		st.Message = "infrastructure not ready yet"
	}
	return st
}

// nodesStep counts the pool's nodes: the NodeClaims launching (no Ready
// condition True), ready, and terminating (deleted), else the MachinePool's
// replicas against its readyReplicas where the cluster cannot be read. Done
// when every node is counted — 0 at scale-to-zero; pending while the pool
// itself is not ready. A ready node that holds nothing is named idle, since
// its last pod left: delete_node_pool removes it with the pool. A Karpenter
// pool's MachinePool that still lists instances the cluster no longer has
// is said so — its list lags by minutes (giantswarm/cluster-manager#49).
func nodesStep(mp, infra *unstructured.Unstructured, live *poolLive, poolReady bool) Step {
	st := Step{Name: StepNodes, State: StepPending}
	if mp == nil {
		return st
	}
	var launching, ready, terminating int
	var since string
	if live != nil {
		for _, claim := range live.claims() {
			cond, found := detect.ReadyCondition(claim)
			switch {
			case claim.GetDeletionTimestamp() != nil:
				terminating++
				since = latest(since, detect.Timestamp(claim.GetDeletionTimestamp().Time))
			case found && cond.Status == "True":
				ready++
				since = latest(since, cond.LastTransitionTime)
			default:
				launching++
				since = latest(since, detect.Timestamp(claim.GetCreationTimestamp().Time))
			}
		}
	}
	replicas, readyReplicas := nestedInt(mp, "spec", "replicas"), nestedInt(mp, "status", "readyReplicas")
	if since == "" {
		since = detect.Timestamp(mp.GetCreationTimestamp().Time)
		if cond, found := detect.ReadyCondition(mp); found && cond.LastTransitionTime != "" {
			since = cond.LastTransitionTime
		}
	}
	st.Since = since
	switch {
	case launching > 0 || terminating > 0:
		st.State = StepInProgress
		st.Message = fmt.Sprintf("%d NodeClaim(s) launching, %d ready, %d terminating", launching, ready, terminating)
	case live != nil && len(live.nodes) == 0 && replicas > 0 && karpenterPool(infra):
		st.State, st.FinishedAt = StepDone, since
		st.Message = fmt.Sprintf("0 nodes on the cluster: the MachinePool still lists %d gone (%s), its list follows within minutes", replicas, joinOrUnknown(providerIDs(infra)))
	case replicas != readyReplicas:
		st.State = StepInProgress
		st.Message = fmt.Sprintf("%d of %d node(s) ready", readyReplicas, replicas)
	case !poolReady:
		st.State = StepPending
	case replicas == 0:
		st.State, st.FinishedAt = StepDone, since
		st.Message = "0 nodes: scale-to-zero, a node launches with the first predictor"
	default:
		st.State, st.FinishedAt = StepDone, since
		st.Message = fmt.Sprintf("%d node(s) ready", readyReplicas)
		if ids := providerIDs(infra); len(ids) > 0 {
			st.Message += " (" + strings.Join(ids, ", ") + ")"
		}
		if live != nil {
			if idle := live.idle(); len(idle) > 0 {
				st.Message += fmt.Sprintf(", %d idle since %s (%s): delete_node_pool removes an idle node with the pool", len(idle), earliestIdle(idle), strings.Join(nodeNames(idle), ", "))
			}
		}
	}
	return st
}

// karpenterPool reports whether a pool's infrastructure is a
// KarpenterMachinePool — the one kind whose nodes come and go with the
// NodeClaims the cluster shows.
func karpenterPool(infra *unstructured.Unstructured) bool {
	return infra != nil && infra.GetKind() == "KarpenterMachinePool"
}

// providerIDs names a pool's nodes by provider id, from its infrastructure
// object's providerIDList (spec, else status).
func providerIDs(infra *unstructured.Unstructured) []string {
	if infra == nil {
		return nil
	}
	ids, _, _ := unstructured.NestedStringSlice(infra.Object, "spec", "providerIDList")
	if len(ids) == 0 {
		ids, _, _ = unstructured.NestedStringSlice(infra.Object, "status", "providerIDList")
	}
	sort.Strings(ids)
	return ids
}

// latest is the later of two RFC3339 timestamps (lexical order holds for
// RFC3339 in UTC); the other when one is empty.
func latest(a, b string) string {
	if a == "" || (b != "" && b > a) {
		return b
	}
	return a
}

// poolReleases lists the cluster's pool releases (HelmReleases of the
// gpu-node-pool chart labelled with the cluster) by release name.
func poolReleases(ctx context.Context, dyn dynamic.Interface, ns, cluster string) (map[string]*unstructured.Unstructured, error) {
	hrs, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: compose.LabelChartName + "=" + compose.PoolChart + "," + compose.LabelCluster + "=" + cluster,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]*unstructured.Unstructured{}, nil
		}
		return nil, fmt.Errorf("list pool releases of %s/%s: %w", ns, cluster, err)
	}
	out := make(map[string]*unstructured.Unstructured, len(hrs.Items))
	for i := range hrs.Items {
		out[hrs.Items[i].GetName()] = &hrs.Items[i]
	}
	return out, nil
}
