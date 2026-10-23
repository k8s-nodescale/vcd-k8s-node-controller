package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/ipam"
)

const (
	defaultHealRetries = 3
	defaultHealBackoff = 2 * time.Minute
)

// policyFor returns the effective reconciliation policy. The CRD default is
// Notify; an empty value means the same, so a status written by 0.3.x behaves.
func policyFor(pool *infrav1.K8sWorkerPool) string {
	switch strings.TrimSpace(pool.Spec.Reconciliation.Policy) {
	case infrav1.ReconciliationAutoHeal:
		return infrav1.ReconciliationAutoHeal
	default:
		return infrav1.ReconciliationNotify
	}
}

func healRetries(pool *infrav1.K8sWorkerPool) int32 {
	if n := pool.Spec.Reconciliation.AutoHealRetries; n > 0 {
		return n
	}
	return defaultHealRetries
}

func healBackoff(pool *infrav1.K8sWorkerPool) time.Duration {
	if d := pool.Spec.Reconciliation.AutoHealBackoff.Duration; d > 0 {
		return d
	}
	return defaultHealBackoff
}

// mergeMissingSlots keeps a worker whose VM disappeared outside the loop. The
// unit is the slot: the address stays in the ledger and the name stays
// claimed, so AutoHeal can recreate the same worker and Notify can report the
// same identity instead of a successor under a new name.
//
// A worker whose removal is already under way is left alone: its VM going away
// is the expected last step, and the removal pipeline finishes the bookkeeping.
func (r *WorkerPoolReconciler) mergeMissingSlots(pool *infrav1.K8sWorkerPool, nodes []infrav1.WorkerNodeStatus, alloc *ipam.Allocator) ([]infrav1.WorkerNodeStatus, error) {
	present := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		present[strings.ToLower(n.Name)] = true
	}

	out := nodes
	for _, prev := range pool.Status.Nodes {
		key := strings.ToLower(prev.Name)
		if present[key] || prev.IP == "" {
			continue
		}
		if err := alloc.Reserve(prev.IP); err != nil {
			return nil, fmt.Errorf("reserve the slot of %s (%s): %w", prev.Name, prev.IP, err)
		}
		if prev.DeleteStartedAt != nil {
			// Removal in flight: the pipeline still owns this entry.
			out = append(out, prev)
			present[key] = true
			continue
		}
		first := prev.Phase != infrav1.NodePhaseMissing
		// Its rollout state describes a VM that no longer exists.
		prev.Resize = nil
		prev.Phase = infrav1.NodePhaseMissing
		prev.K8sReady = false
		prev.ProviderID = ""
		prev.Message = fmt.Sprintf("VM is missing from vCD; slot %s (%s) stays reserved (reconciliation.policy=%s)",
			prev.Name, prev.IP, policyFor(pool))
		out = append(out, prev)
		present[key] = true
		if first {
			r.eventf(pool, corev1.EventTypeWarning, "WorkerMissing",
				fmt.Sprintf("worker %s (%s) is missing from vCD; slot reserved", prev.Name, prev.IP))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// reconcileMissing acts on slots whose VM is gone. Notify reports them through
// the pool condition and never creates anything; AutoHeal recreates the same
// name at the same address with a bound and a backoff.
func (r *WorkerPoolReconciler) reconcileMissing(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator) (bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var missing []int
	for i := range pool.Status.Nodes {
		n := &pool.Status.Nodes[i]
		if n.Phase == infrav1.NodePhaseMissing && n.DeleteStartedAt == nil {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return false, ctrl.Result{}, nil
	}
	if policyFor(pool) != infrav1.ReconciliationAutoHeal {
		// evaluatePoolCondition turns this into Ready=False/WorkerMissing.
		return false, ctrl.Result{}, nil
	}

	retries := healRetries(pool)
	backoff := healBackoff(pool)
	var next time.Duration
	for _, i := range missing {
		n := &pool.Status.Nodes[i]

		if n.HealAttempts >= retries {
			if n.Phase != infrav1.NodePhaseFailed {
				n.Phase = infrav1.NodePhaseFailed
				n.Message = fmt.Sprintf("AutoHeal gave up after %d attempt(s); the slot stays reserved", n.HealAttempts)
				r.eventf(pool, corev1.EventTypeWarning, "AutoHealFailed",
					fmt.Sprintf("worker %s: %s", n.Name, n.Message))
			}
			continue
		}
		if n.HealLastAttempt != nil {
			if wait := backoff - time.Since(n.HealLastAttempt.Time); wait > 0 {
				if next == 0 || wait < next {
					next = wait
				}
				continue
			}
		}

		vm, err := r.createWorker(ctx, pool, alloc, n.Name, n.IP)
		now := metav1.Now()
		n.HealAttempts++
		n.HealLastAttempt = &now
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		if err != nil {
			n.Message = "AutoHeal attempt failed: " + err.Error()
			logger.Error(err, "AutoHeal failed", "worker", n.Name, "ip", n.IP)
			r.eventf(pool, corev1.EventTypeWarning, "AutoHealFailed",
				fmt.Sprintf("worker %s (%s): %v", n.Name, n.IP, err))
			continue
		}
		n.ProviderID = vm.ID
		n.Phase = infrav1.NodePhaseProvisioning
		n.K8sReady = false
		n.Message = ""
		r.eventf(pool, corev1.EventTypeNormal, "WorkerHealed",
			fmt.Sprintf("recreated worker %s (%s) in its reserved slot (attempt %d/%d)", n.Name, n.IP, n.HealAttempts, retries))
	}

	if next <= 0 {
		next = requeueProgressing
	}
	return true, ctrl.Result{RequeueAfter: next}, nil
}

// healLostNode power-cycles a worker whose VM is alive but whose Node object
// disappeared. One reboot makes kubelet re-register; anything more is bounded by
// the same retry budget as a missing VM, so a broken guest parks as Failed
// instead of being rebooted forever.
func (r *WorkerPoolReconciler) healLostNode(ctx context.Context, pool *infrav1.K8sWorkerPool, st *infrav1.WorkerNodeStatus) {
	logger := log.FromContext(ctx)

	if st.HealAttempts >= healRetries(pool) {
		st.Phase = infrav1.NodePhaseFailed
		st.Message = fmt.Sprintf("AutoHeal gave up on the lost Node registration after %d attempt(s)", st.HealAttempts)
		r.eventf(pool, corev1.EventTypeWarning, "AutoHealFailed",
			fmt.Sprintf("worker %s: %s", st.Name, st.Message))
		return
	}
	backoff := healBackoff(pool)
	if st.HealLastAttempt != nil && time.Since(st.HealLastAttempt.Time) < backoff {
		st.Message = "lost its Node registration; waiting for the AutoHeal backoff"
		return
	}
	if err := r.Cloud.CycleVM(ctx, pool.Spec.VCD.VApp, st.ProviderID); err != nil {
		st.Message = "AutoHeal power-cycle failed: " + err.Error()
		logger.Error(err, "AutoHeal power-cycle failed", "worker", st.Name)
		r.eventf(pool, corev1.EventTypeWarning, "AutoHealFailed",
			fmt.Sprintf("worker %s: power-cycle failed: %v", st.Name, err))
		return
	}
	now := metav1.Now()
	st.HealLastAttempt = &now
	st.HealAttempts++
	st.Message = "power-cycled the VM to re-register its Node (AutoHeal)"
	r.eventf(pool, corev1.EventTypeNormal, "WorkerRebooted",
		fmt.Sprintf("worker %s: VM power-cycled to re-register its Node object", st.Name))
}

// workerServes reports whether a slot can serve workloads now or once its
// current step finishes. A Missing slot has no VM, a Failed slot is parked, and
// a slot whose removal is under way is on its way out: none of them may be
// counted when the policy asks how many workers the pool would keep.
func workerServes(node *infrav1.WorkerNodeStatus) bool {
	if node.DeleteStartedAt != nil {
		return false
	}
	switch node.Phase {
	case infrav1.NodePhaseMissing, infrav1.NodePhaseFailed,
		infrav1.NodePhaseDraining, infrav1.NodePhaseDeleting:
		return false
	}
	return true
}

// usableWorkers counts the slots that can serve workloads.
func usableWorkers(nodes []infrav1.WorkerNodeStatus) int32 {
	var n int32
	for i := range nodes {
		if workerServes(&nodes[i]) {
			n++
		}
	}
	return n
}

// removalGuard decides whether one worker may be removed.
//
// slotsAfter is the number of slots left once it is gone, so zero means the
// request would empty the pool. removesUsable says the worker still counts as
// able to serve, which is what makes its removal a real loss of capacity.
//
// minReplicas is a floor on workers able to serve, not on status rows: writing
// off a slot whose VM is already gone takes nothing out of service, so the
// floor never blocks that cleanup. Emptying the pool still needs the explicit
// opt-in, because the pool would be left with nothing at all.
//
// Two refusals survive scaleDown.allowLastWorker because they are not policy:
// the worker this controller runs on, and (checked by the caller) node-local
// volumes that hold data.
func (r *WorkerPoolReconciler) removalGuard(ctx context.Context, pool *infrav1.K8sWorkerPool, kubeName string, slotsAfter int, removesUsable bool, usableAfter int32) (reason, msg string) {
	if minReplicas := pool.Spec.ScaleDown.MinReplicas; minReplicas > 0 && removesUsable && usableAfter < minReplicas {
		return "MinReplicasProtected", fmt.Sprintf(
			"refusing to delete %s: %d worker(s) able to serve would fall below scaleDown.minReplicas=%d",
			kubeName, usableAfter, minReplicas)
	}
	if slotsAfter > 0 {
		return "", ""
	}
	// The two explicit conditions: minReplicas: 0 and the flag.
	if pool.Spec.ScaleDown.MinReplicas > 0 || !pool.Spec.ScaleDown.AllowLastWorker {
		return "LastWorkerProtected", fmt.Sprintf(
			"refusing to delete %s: this would leave the pool with no workers; emptying the pool needs scaleDown.minReplicas: 0 and scaleDown.allowLastWorker: true",
			kubeName)
	}
	// Not policy: the pass would die with the worker it is deleting.
	if own := r.ownNodeName(ctx); own != "" && kubeName != "" && strings.EqualFold(own, kubeName) {
		return "WorkerHostsController", fmt.Sprintf(
			"refusing to delete %s: this controller runs on that worker, so the pass would not finish the removal; move the controller to the control plane first",
			kubeName)
	}
	return "", ""
}

// ownNodeName is the Node this controller runs on, from the downward API.
// An empty result means the controller is not running in a Pod (tests, or a
// binary started by hand), in which case the guard cannot apply.
func (r *WorkerPoolReconciler) ownNodeName(ctx context.Context) string {
	if r.ownNode != nil {
		return *r.ownNode
	}
	own := ""
	name, ns := r.PodName, r.PodNamespace
	if name != "" {
		var pod corev1.Pod
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod); err == nil {
			own = pod.Spec.NodeName
		}
	}
	r.ownNode = &own
	return own
}
