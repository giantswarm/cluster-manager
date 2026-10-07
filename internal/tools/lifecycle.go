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
	// jobFailed is the Job's terminal condition for a Job that did not
	// complete.
	jobFailed = "Failed"
)

// The pod phases of a pod that has ended.
const (
	podSucceeded = "Succeeded"
	podFailed    = "Failed"
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
	// launch is what the release says about the nodes Karpenter refused to
	// launch — sizes, zones, the cache claim behind a pin —; empty while it
	// launches them.
	launch launchContext
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
func (s *Service) readPoolState(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, t target, mp, release *unstructured.Unstructured, pool string, clusterZones []string) *poolState {
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
			if live := readPoolLive(gctx, t.Reader, pool, gpuPoolRelease(release)); live.readable() {
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
	r.launch = s.launchContext(ctx, t, r.release, r.live, clusterZones)
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
	var release Step
	if r.release != nil {
		release = releaseStep(r.release)
		steps = append(steps, release)
		removing = removing || r.release.GetDeletionTimestamp() != nil
	}
	steps = append(steps, machinePoolStep(r.mp))
	if r.mp != nil && r.mp.GetDeletionTimestamp() != nil {
		removing = true
	}
	steps = append(steps, nodesStep(r.mp, r.infra, r.live, steps[len(steps)-1].State == StepDone, gpuPoolRelease(r.release), removing, r.launch))
	phase := poolPhase(steps)
	if r.prewarm != nil {
		steps = append(steps, prewarmStep(r.prewarm, release.State == StepDone, r.live.refusal(r.launch), r.live))
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
// The chart creates the Job with the release's install only, so an absent
// Job while the pool release is not Ready (releaseDone false) says nothing
// about the placeholder — the install has not happened: the step is pending
// until the release step is done (giantswarm/cluster-manager#53). A Job that
// exists speaks for itself whatever the release reports. Its terminal
// condition decides where it has one: Complete — the hold ended without a
// workload, done; Failed for DeadlineExceeded — no node came within the hold
// plus ten minutes, failed; Failed otherwise — the pod ended before its hold,
// preempted by the first workload (backoffLimit 0: it is not replaced), done.
// While the Job is active its pod says: Pending while the first node
// launches — or while Karpenter cannot launch it, the refusal (the nodes
// step's, in a line) named instead of the scheduler's message
// (giantswarm/cluster-manager#55) —, Running while it holds the node,
// terminating while the first workload takes it. A Job that is gone after
// the release installed — its TTL removed it ten minutes after it ended, or
// prewarm was set on an existing pool — is done, saying so.
//
// The pool's nodes (live) speak beside the Job and its pod
// (giantswarm/cluster-manager#85): the Job and its pod alone mis-described a
// healthy pool twice. A node joins minutes before it advertises its GPU (the
// GPU operator installs the driver and the toolkit, then the device plugin
// registers nvidia.com/gpu), so a pod Pending on a pool whose node has joined
// waits for the GPU, not for the node, and a pod the node's kubelet rejected
// in that window (a Failed pod with the kubelet's reason, not a preemption —
// a preempted pod is deleted) left the Job Failed under backoffLimit 0 while
// the node came up: the step waits for the GPU while the node advertises
// none, and is done once it does. A workload holding a node of the pool (a
// GPU pod or a KServe predictor — what makes a node busy, see holders) is the
// placeholder's purpose reached, whatever the Job or its pod say: the first
// model took the pool's GPU, possibly before the placeholder was placed,
// which then waits behind it at negative priority until its deadline; done,
// naming the workload. A failure stays one where it is one: the placeholder's
// container failed, or its pod was rejected and the pool has no node left.
func prewarmStep(p *prewarmState, releaseDone bool, refusal string, live *poolLive) Step {
	st := Step{Name: StepPrewarm, State: StepDone}
	job := p.namespace + "/" + p.name
	if p.job == nil {
		if !releaseDone {
			st.State = StepPending
			st.Message = "the pool release is not Ready yet: the placeholder Job " + job + " is created with the release's install (the release step says where it stands)"
			return st
		}
		st.Message = "no placeholder Job " + job + ": removed ten minutes after it ended, or prewarm was set on an existing pool (the Job is created with the release's install only)"
		return st
	}
	st.Since = detect.Timestamp(p.job.GetCreationTimestamp().Time)
	if cond, found := detect.ConditionOf(p.job, "Complete"); found && cond.Status == "True" {
		st.FinishedAt = latest(cond.LastTransitionTime, nestedString(p.job, "status", "completionTime"))
		st.Message = "finished: the hold ended without a workload; Karpenter consolidates the empty node"
		return st
	}
	served, joined := placeholderNodes(live)
	pod := newestPod(p.pods)
	if cond, found := detect.ConditionOf(p.job, jobFailed); found && cond.Status == "True" {
		st.FinishedAt = cond.LastTransitionTime
		detail := strings.TrimSpace(cond.Reason + " " + strings.Join(strings.Fields(cond.Message), " "))
		if served != nil {
			st.Message = servedMessage(served) + "; the placeholder Job has ended (" + detail + ")"
			return st
		}
		if why, rejected, failed := podFailure(pod); failed {
			return failedPodStep(st, pod, why, rejected, joined)
		}
		if cond.Reason == jobReasonDeadline {
			st.State = StepFailed
			st.Message = fmt.Sprintf("no node came within the placeholder's deadline of %d s (%s): ", nestedInt(p.job, "spec", "activeDeadlineSeconds"), detail)
			if refusal != "" {
				st.Message += "Karpenter could not launch one — " + refusal + " (the nodes step carries its message)"
			} else {
				st.Message += "check the pool's NodeClaims and Karpenter's log"
			}
			return st
		}
		st.Message = "preempted: the first workload took the placeholder's node before its hold ended (" + detail + ")"
		return st
	}
	st.State = StepInProgress
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
	case served != nil:
		st.State, st.FinishedAt = StepDone, latest(st.Since, served.joinedAt())
		st.Message = servedMessage(served) + "; the placeholder's pod " + pod.GetName() + " (" + phase + ") waits behind it at negative priority until the Job's deadline"
	case phase == podFailed:
		// The Job has not concluded on it yet: the controller marks the Job
		// Failed within seconds of its pod's failure.
		why, rejected, _ := podFailure(pod)
		return failedPodStep(st, pod, why, rejected, joined)
	case phase == "Pending" && node != "" && !unscheduled(pod):
		st.Since = latest(st.Since, detect.Timestamp(pod.GetCreationTimestamp().Time))
		st.Message = "starting on node " + node + ": the scheduler placed the placeholder, its container is being created"
	case phase == "Pending" && joined != nil:
		st.Since = latest(st.Since, latest(detect.Timestamp(pod.GetCreationTimestamp().Time), joined.joinedAt()))
		st.Message = waitingMessage(joined)
		if gpus := joined.gpus(); gpus > 0 {
			st.Message = fmt.Sprintf("pending: the pool's node %s advertises %d %s, the scheduler has not placed the placeholder yet", joined.name(), gpus, detect.GPUResource)
		}
		if cond, found := detect.ConditionOf(pod, "PodScheduled"); found && cond.Status != "True" && cond.Message != "" {
			st.Message += " (" + strings.Join(strings.Fields(cond.Message), " ") + ")"
		}
	case phase == "Pending" && refusal != "":
		st.Since = latest(st.Since, detect.Timestamp(pod.GetCreationTimestamp().Time))
		st.Message = "pending: the placeholder waits for the pool's first node, which Karpenter could not launch — " + refusal + " (the nodes step carries Karpenter's message)"
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

// placeholderNodes reads the pool's registered nodes for the prewarm step:
// served is the first a workload holds (holders: a GPU pod or a KServe
// predictor; the placeholder itself, at negative priority, never counts),
// joined the first registered one; either nil where the pool has none, both
// nil where the cluster cannot be read. A terminating node is Karpenter's and
// speaks for neither.
func placeholderNodes(live *poolLive) (served, joined *poolNode) {
	if live == nil {
		return nil, nil
	}
	for _, n := range live.nodes {
		if n.node == nil || n.terminating() {
			continue
		}
		if served == nil && len(n.holders) > 0 {
			served = n
		}
		if joined == nil {
			joined = n
		}
	}
	return served, joined
}

// servedMessage names the workload on the pool's node: the placeholder's
// purpose, reached.
func servedMessage(n *poolNode) string {
	return "the first workload runs on the pool's node " + n.name() + ": " + strings.Join(n.holders, ", ")
}

// waitingMessage says the placeholder waits for the node's GPU: the node has
// joined, its device plugin has not advertised nvidia.com/gpu yet.
func waitingMessage(n *poolNode) string {
	return fmt.Sprintf("waiting for the pool's node %s to advertise its GPU: it joined at %s, its device plugin has not registered %s yet (the GPU operator installs the driver and the container toolkit first)", n.name(), n.joinedAt(), detect.GPUResource)
}

// podFailure reads why the placeholder's pod failed: rejected when the
// node's kubelet refused it at admission (the pod's status.reason — for a GPU
// pod UnexpectedAdmissionError while the device plugin is not serving the
// resource), else the hold container's termination. failed is false for no
// pod, one still running or being deleted, and one the scheduler or the node
// disrupted (a DisruptionTarget condition: preemption, eviction) — those are
// the preemption the step already words.
func podFailure(pod *unstructured.Unstructured) (why string, rejected, failed bool) {
	if pod == nil || pod.GetDeletionTimestamp() != nil || nestedString(pod, "status", "phase") != podFailed {
		return "", false, false
	}
	if cond, found := detect.ConditionOf(pod, "DisruptionTarget"); found && cond.Status == "True" {
		return "", false, false
	}
	if reason := nestedString(pod, "status", "reason"); reason != "" {
		return strings.TrimSpace(reason + ": " + strings.Join(strings.Fields(nestedString(pod, "status", "message")), " ")), true, true
	}
	statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
	for _, s := range statuses {
		if m, ok := s.(map[string]any); ok {
			if term, found, _ := unstructured.NestedMap(m, "state", "terminated"); found {
				reason, _ := term["reason"].(string)
				code, _ := number(term["exitCode"])
				return fmt.Sprintf("container %v terminated: %s, exit code %d", m["name"], reason, int64(code)), false, true
			}
		}
	}
	return "the pod failed without a reason recorded", false, true
}

// failedPodStep words a placeholder whose pod failed. Rejected by the node's
// kubelet — the window between the node joining and its GPU being advertised
// —, the node decides: still advertising no GPU, the step waits for it; the
// GPU advertised, the node is up and the step done (the Job does not replace
// the pod, backoffLimit 0, so nothing holds the node until the first
// workload); no node left, failed. The hold container failing is a failure
// whatever the node.
func failedPodStep(st Step, pod *unstructured.Unstructured, why string, rejected bool, joined *poolNode) Step {
	node := nestedString(pod, "spec", "nodeName")
	switch {
	case !rejected:
		st.State = StepFailed
		st.Message = "the placeholder's pod " + pod.GetName() + " failed on node " + node + " (" + why + "): check the placeholder image and the node's kubelet log"
	case joined != nil && joined.gpus() == 0:
		st.State, st.FinishedAt = StepInProgress, ""
		st.Since = latest(st.Since, joined.joinedAt())
		st.Message = waitingMessage(joined) + "; the node rejected the placeholder's pod " + pod.GetName() + " meanwhile (" + why + "), which the Job does not replace (backoffLimit 0): the node stays, and the first workload is placed on it once the GPU is advertised"
	case joined != nil:
		st.State, st.FinishedAt = StepDone, latest(st.FinishedAt, latest(st.Since, joined.joinedAt()))
		st.Message = fmt.Sprintf("the pool's node %s is up and advertises %d %s; it rejected the placeholder's pod %s before (%s), which the Job does not replace (backoffLimit 0): nothing holds the node until the first workload, and Karpenter consolidates it if it stays empty", joined.name(), joined.gpus(), detect.GPUResource, pod.GetName(), why)
	default:
		st.State = StepFailed
		st.Message = "the placeholder's pod " + pod.GetName() + " was rejected by node " + node + " (" + why + ") and the pool has no node now: the first predictor launches one"
	}
	return st
}

// unscheduled: the pod's PodScheduled condition is False — the scheduler
// has not placed it, whatever spec.nodeName says.
func unscheduled(pod *unstructured.Unstructured) bool {
	cond, found := detect.ConditionOf(pod, "PodScheduled")
	return found && cond.Status == "False"
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
// itself is not ready. A node Karpenter could not launch — a NodeClaim
// carrying its refusal, or the refusal event of a claim it deleted at once
// for want of capacity — keeps the step in progress, with the refusal in
// Karpenter's words, since the last one: the pool has no node for the pod
// that waits, and it must not read done with none (giantswarm/cluster-manager#55).
// On a GPU pool of cluster-manager's (gpuPool), a ready node that holds
// nothing is named idle, since its last pod left — delete_node_pool removes
// it with the pool —, and a MachinePool that still lists instances the
// cluster no longer has is said so, its list lags by minutes
// (giantswarm/cluster-manager#49); any other pool's nodes carry the
// cluster's workloads and are never idle in that sense. While the pool is
// removed (removing) and the cluster is readable, its NodeClaims and Nodes
// are all the step reads: the MachinePool's replicas lag a gone NodeClaim by
// minutes and read a node ready after it went — no NodeClaim, no node
// (giantswarm/cluster-manager#190).
func nodesStep(mp, infra *unstructured.Unstructured, live *poolLive, poolReady, gpuPool, removing bool, lc launchContext) Step {
	st := Step{Name: StepNodes, State: StepPending}
	if mp == nil {
		return st
	}
	var launching int
	var ready, terminating []*poolNode
	var since string
	if live != nil {
		for _, n := range live.nodes {
			if n.claim == nil || live.refused(n.claim) {
				continue
			}
			cond, found := detect.ReadyCondition(n.claim)
			switch {
			case n.terminating():
				terminating = append(terminating, n)
				since = latest(since, n.deletedAt())
			case found && cond.Status == "True":
				ready = append(ready, n)
				since = latest(since, cond.LastTransitionTime)
			default:
				launching++
				since = latest(since, detect.Timestamp(n.claim.GetCreationTimestamp().Time))
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
	case live != nil && len(live.failures) > 0:
		st.State = StepInProgress
		st.Since = live.failures[len(live.failures)-1].at
		st.Message = launchFailureMessage(live.failures, launching, len(ready), len(terminating), lc)
	case launching > 0 || len(terminating) > 0:
		st.State = StepInProgress
		st.Message = fmt.Sprintf("%d NodeClaim(s) launching, %d ready, %d terminating", launching, len(ready), len(terminating))
		if len(terminating) > 0 {
			st.Message += " — " + terminatingNodes(terminating)
		}
	case removing && live != nil && len(ready) > 0:
		st.State = StepInProgress
		st.Message = fmt.Sprintf("%d node(s) ready (%s): their NodeClaims go with the pool's release", len(ready), strings.Join(nodeNames(ready), ", "))
	case removing && live != nil && replicas > 0:
		st.State, st.FinishedAt = StepDone, since
		st.Message = fmt.Sprintf("0 nodes on the cluster: the MachinePool still lists %d gone (%s), its list follows within minutes", replicas, joinOrUnknown(providerIDs(infra)))
	case removing && live != nil:
		st.State, st.FinishedAt = StepDone, since
		st.Message = "0 nodes on the cluster"
	case gpuPool && live != nil && len(live.nodes) == 0 && replicas > 0:
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
		if gpuPool && live != nil {
			if idle := live.idle(); len(idle) > 0 {
				st.Message += fmt.Sprintf(", %d idle since %s (%s): delete_node_pool removes an idle node with the pool", len(idle), earliestIdle(idle), strings.Join(nodeNames(idle), ", "))
			}
		}
	}
	return st
}

// terminatingNodes names the nodes whose NodeClaim is deleted, each since
// when and where its termination stands (terminationStage): the claim —
// with it the MachinePool of a pool being removed — is gone once EC2
// confirms the termination. The person watching a pool go sees what is
// still going and whether it is still billed, not a bare count
// (giantswarm/cluster-manager#57).
func terminatingNodes(nodes []*poolNode) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("%s terminating since %s (%s)", n.name(), n.deletedAt(), n.terminationStage()))
	}
	return strings.Join(parts, "; ")
}

// gpuPoolRelease reports whether hr is a GPU pool release of
// cluster-manager's chart (gpu-node-pool) — the pools whose nodes are idle
// or busy by their GPU workload.
func gpuPoolRelease(hr *unstructured.Unstructured) bool {
	return hr != nil && hr.GetLabels()[compose.LabelChartName] == compose.PoolChart
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
