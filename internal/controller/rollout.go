package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/ipam"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/kube"
)

// Per-worker rollout phases, persisted in status.nodes[].resize.phase so a
// controller restart resumes where the previous pass stopped.
const (
	resizePhaseCordoned = "Cordoned"
	resizePhaseStopped  = "Stopped"
	resizePhaseBooting  = "Booting"
	resizePhaseFailed   = "Failed"
)

const defaultRolloutReadyTimeout = 10 * time.Minute

// flavorDelta is how a VM's actual shape compares with spec.flavor.
type flavorDelta int

const (
	flavorEqual flavorDelta = iota
	flavorGrow
	flavorShrink
	flavorMixed
)

// compareFlavor compares what a VM has with what the spec wants. Zero means
// "no opinion" on either side, so an unobserved value never triggers work.
func compareFlavor(have, want *infrav1.FlavorStatus) flavorDelta {
	if have == nil || want == nil {
		return flavorEqual
	}
	grow, shrink := false, false
	cmp := func(h, w int32) {
		if h <= 0 || w <= 0 || h == w {
			return
		}
		if w > h {
			grow = true
		} else {
			shrink = true
		}
	}
	cmp(have.CPUs, want.CPUs)
	cmp(have.MemoryMB, want.MemoryMB)
	cmp(have.OSDiskGB, want.OSDiskGB)
	switch {
	case grow && shrink:
		return flavorMixed
	case shrink:
		return flavorShrink
	case grow:
		return flavorGrow
	default:
		return flavorEqual
	}
}

func rolloutMaxUnavailable(pool *infrav1.K8sWorkerPool, workers int) int {
	v := pool.Spec.Rollout.MaxUnavailable
	if v.IntVal == 0 && v.StrVal == "" {
		return 1
	}
	n, err := intstr.GetScaledValueFromIntOrPercent(&v, workers, true)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func rolloutReadyTimeout(pool *infrav1.K8sWorkerPool) time.Duration {
	if d := pool.Spec.Rollout.ReadyTimeout.Duration; d > 0 {
		return d
	}
	return defaultRolloutReadyTimeout
}

func rolloutPauseOnFailure(pool *infrav1.K8sWorkerPool) bool {
	if p := pool.Spec.Rollout.PauseOnFailure; p != nil {
		return *p
	}
	return true
}

func rolloutInFlight(pool *infrav1.K8sWorkerPool) int {
	n := 0
	for i := range pool.Status.Nodes {
		r := pool.Status.Nodes[i].Resize
		if r != nil && r.Phase != resizePhaseFailed {
			n++
		}
	}
	return n
}

// failedResizeHealable reports whether a slot left in Failed can be closed out
// on this pass: the VM carries the target shape, so only the closing steps
// (uncordon, ingress pool) are missing.
func failedResizeHealable(pool *infrav1.K8sWorkerPool, target *infrav1.FlavorStatus) bool {
	for i := range pool.Status.Nodes {
		n := &pool.Status.Nodes[i]
		if n.Resize != nil && n.Resize.Phase == resizePhaseFailed && compareFlavor(n.Flavor, target) == flavorEqual {
			return true
		}
	}
	return false
}

// rolloutBlocksRemoval is true only while a roll is actually making progress. A
// paused (failed) roll must not lock the pool: deleting the failed worker is the
// documented escape, and it has to be allowed.
func rolloutBlocksRemoval(pool *infrav1.K8sWorkerPool) bool {
	return pool.Status.Rollout.Phase == infrav1.RolloutPhaseProgressing && rolloutInFlight(pool) > 0
}

// reconcileRollout applies spec.flavor to existing workers one batch at a time:
// cordon, remove from the ingress pool, drain, wait for volumes to detach,
// power off, resize, power on, wait for Ready, uncordon, put back. Each pass
// advances a worker by one step and persists that step in status, so a
// controller restart during a roll resumes instead of starting over.
//
// It returns handled=true while a roll is in progress, which keeps the pass from
// overwriting the rollout conditions with the converged state.
func (r *WorkerPoolReconciler) reconcileRollout(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator) (bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)

	target := &infrav1.FlavorStatus{
		CPUs:     pool.Spec.Flavor.CPUs,
		MemoryMB: pool.Spec.Flavor.MemoryMB,
		OSDiskGB: pool.Spec.Flavor.OSDiskGB,
	}

	inFlight := rolloutInFlight(pool)
	var pending, unsupported []int
	for i := range pool.Status.Nodes {
		n := &pool.Status.Nodes[i]
		if n.Resize != nil || n.Phase == infrav1.NodePhaseMissing ||
			n.Phase == infrav1.NodePhaseFailed || n.DeleteStartedAt != nil || n.Flavor == nil {
			continue
		}
		switch compareFlavor(n.Flavor, target) {
		case flavorEqual:
			continue
		case flavorShrink, flavorMixed:
			unsupported = append(unsupported, i)
		case flavorGrow:
			pending = append(pending, i)
		}
	}

	// A Failed slot is not in flight, but it may still be closable when the VM
	// already carries the target shape and only a closing step failed.
	healable := failedResizeHealable(pool, target)

	if inFlight == 0 && len(pending) == 0 && !healable {
		if len(unsupported) > 0 {
			var names []string
			for _, i := range unsupported {
				names = append(names, pool.Status.Nodes[i].Name)
			}
			msg := fmt.Sprintf("spec.flavor is smaller than %d worker(s) (%s); vCD cannot shrink a VM, so this needs a recreate per worker and is not implemented yet",
				len(names), strings.Join(names, ", "))
			r.setCondition(pool, metav1.ConditionFalse, "UnsupportedChange", msg)
			r.eventf(pool, corev1.EventTypeWarning, "UnsupportedChange", msg)
			return true, ctrl.Result{RequeueAfter: requeueConverged}, nil
		}
		if pool.Status.Rollout.Phase != "" && pool.Status.Rollout.Phase != infrav1.RolloutPhaseIdle {
			pool.Status.Rollout = infrav1.RolloutStatus{Phase: infrav1.RolloutPhaseIdle}
			r.eventf(pool, corev1.EventTypeNormal, "RolloutComplete", "every worker matches spec.flavor")
			return true, ctrl.Result{RequeueAfter: requeueConverged}, nil
		}
		return false, ctrl.Result{}, nil
	}

	// Manual strategy: only workers an operator opted in are touched.
	if strings.EqualFold(pool.Spec.Rollout.Strategy, infrav1.RolloutStrategyManual) && len(pending) > 0 {
		allowed, err := r.resizeAllowed(ctx)
		if err != nil {
			return false, ctrl.Result{}, err
		}
		kept := pending[:0]
		for _, i := range pending {
			if allowed[strings.ToLower(pool.Status.Nodes[i].Name)] {
				kept = append(kept, i)
			}
		}
		pending = kept
	}

	// Order: highest ordinal first by default, so wrk-01 moves last and the
	// pool keeps its longest-lived workers until the end.
	if !strings.EqualFold(pool.Spec.Rollout.Order, infrav1.RolloutOrderAscending) {
		for i, j := 0, len(pending)-1; i < j; i, j = i+1, j-1 {
			pending[i], pending[j] = pending[j], pending[i]
		}
	}

	wasPhase := pool.Status.Rollout.Phase
	maxUnavailable := rolloutMaxUnavailable(pool, len(pool.Status.Nodes))
	now := metav1.Now()

	// Capacity pre-flight: a resize consumes vDC capacity the same way a new
	// worker does, so a roll that cannot fit must not drain a worker first and
	// find out on the power-on that it cannot be resized. The check covers the
	// batch this pass would start; workers already in flight are past it.
	start := pending
	// maxUnavailable can shrink under a roll that is already wider than the new
	// limit, so room may be negative: start nothing, keep the rest pending.
	if room := maxUnavailable - inFlight; room <= 0 {
		start = nil
	} else if len(start) > room {
		start = start[:room]
	}
	queuedWhy := ""
	if len(start) > 0 && capacityPreflightEnabled(pool) {
		capacity, err := r.Cloud.Capacity(ctx)
		switch {
		case err != nil:
			logger.V(1).Info("capacity pre-flight skipped: the vDC capacity could not be read",
				"pool", pool.Name, "error", err)
		default:
			mhz := cpuMHzPerCore(pool, capacity)
			var need cloud.CapacityRequest
			var fits []int
			for _, i := range start {
				next := addCapacityRequest(need, resizeCapacityRequest(pool.Status.Nodes[i].Flavor, target))
				if ok, short := capacity.Fits(next, mhz); !ok {
					queuedWhy = strings.Join(short, "; ")
					break
				}
				need = next
				fits = append(fits, i)
			}
			start = fits
		}
	}
	for _, i := range start {
		n := &pool.Status.Nodes[i]
		n.Resize = &infrav1.ResizeStatus{Phase: resizePhaseCordoned, StartedAt: &now}
		n.Phase = infrav1.NodePhaseResizing
		n.Message = "rollout: taking the worker out of service"
		inFlight++
		logger.Info("rollout: started", "worker", n.Name, "have", n.Flavor, "want", target)
	}

	if pool.Status.Rollout.StartedAt == nil {
		pool.Status.Rollout.StartedAt = &now
	}

	failed := false
	for i := range pool.Status.Nodes {
		n := &pool.Status.Nodes[i]
		if n.Resize == nil {
			continue
		}
		if n.Resize.Phase == resizePhaseFailed {
			// A step failed, but the VM may already carry the target shape - the
			// pool add at the very end is the usual culprit. Retry the closing
			// steps, which are idempotent, instead of freezing the slot in
			// Resizing/Failed for good.
			if err := r.advanceResize(ctx, pool, n, target); err == nil && n.Resize == nil {
				continue
			}
			failed = true
			continue
		}
		if err := r.advanceResize(ctx, pool, n, target); err != nil {
			// A drain that cannot finish yet is progress, not a failure: the
			// pass retries and the PDB is what decides.
			if errors.Is(err, kube.ErrDrainIncomplete) {
				n.Resize.Message = "drain in progress (a PodDisruptionBudget or an unmovable pod is holding the node)"
				continue
			}
			n.Resize.Phase = resizePhaseFailed
			n.Resize.Message = err.Error()
			failed = true
			logger.Error(err, "rollout: step failed", "worker", n.Name)
			r.eventf(pool, corev1.EventTypeWarning, "RolloutFailed",
				fmt.Sprintf("worker %s: %v", n.Name, err))
		}
	}

	phase := infrav1.RolloutPhaseProgressing
	switch {
	case failed:
		phase = infrav1.RolloutPhasePaused
	case queuedWhy != "" && inFlight == 0:
		// Nothing has been taken out of service: the roll waits for capacity.
		phase = infrav1.RolloutPhaseQueued
	}
	message := ""
	if queuedWhy != "" {
		message = "waiting for vDC capacity: " + queuedWhy
	}
	current := ""
	done := 0
	for i := range pool.Status.Nodes {
		n := &pool.Status.Nodes[i]
		if n.Resize != nil && current == "" {
			current = n.Name
		}
		if n.Resize == nil && n.Flavor != nil && compareFlavor(n.Flavor, target) == flavorEqual &&
			n.Phase == infrav1.NodePhaseReady {
			done++
		}
	}
	pool.Status.Rollout = infrav1.RolloutStatus{
		Phase:         phase,
		Message:       message,
		TargetFlavor:  target,
		WorkersTotal:  int32(done + inFlight),
		WorkersDone:   int32(done),
		CurrentWorker: current,
		StartedAt:     pool.Status.Rollout.StartedAt,
	}
	if failed {
		pool.Status.Rollout.LastError = "a worker failed its rollout step; see the RolloutFailed event"
	}
	if phase == infrav1.RolloutPhaseQueued {
		msg := fmt.Sprintf("%d worker(s) still need the target shape; %s", len(pending), message)
		r.setCondition(pool, metav1.ConditionFalse, "NoResourceAvailable", msg)
		if wasPhase != infrav1.RolloutPhaseQueued {
			r.eventf(pool, corev1.EventTypeWarning, "RolloutQueued", msg)
		}
	}
	return true, ctrl.Result{RequeueAfter: requeueProgressing}, nil
}

// advanceResize moves one worker one step through the rollout.
func (r *WorkerPoolReconciler) advanceResize(ctx context.Context, pool *infrav1.K8sWorkerPool, n *infrav1.WorkerNodeStatus, target *infrav1.FlavorStatus) error {
	kubeName := n.NodeName
	if kubeName == "" {
		kubeName = strings.ToLower(n.Name)
	}

	switch n.Resize.Phase {
	case resizePhaseCordoned, "":
		if err := r.Kube.Cordon(ctx, kubeName); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("cordon %s: %w", kubeName, err)
		}
		if err := r.removeFromIngressPool(ctx, pool, n.IP); err != nil && !errors.Is(err, cloud.ErrPoolNotConfigured) {
			return fmt.Errorf("remove %s from the ingress pool: %w", n.IP, err)
		}
		if err := r.Kube.Drain(ctx, kubeName, budgetFor(pool)); err != nil {
			return err
		}
		if err := r.Kube.WaitVolumesDetached(ctx, kubeName, volumeWaitTimeout); err != nil {
			return err
		}
		// Read the VM before powering anything off. A step that a status
		// conflict re-drove can find the resize already finished, and a VM that
		// already carries the target shape must not be power-cycled again
		// (I-10).
		vm, err := r.Cloud.GetVM(ctx, pool.Spec.VCD.VApp, n.ProviderID)
		if err != nil {
			return fmt.Errorf("read %s: %w", n.Name, err)
		}
		have := &infrav1.FlavorStatus{CPUs: vm.CPU, MemoryMB: vm.MemoryMB, OSDiskGB: vm.OSDiskGB}
		if compareFlavor(have, target) == flavorEqual {
			n.Flavor = have
			if !vm.PoweredOn {
				if err := r.Cloud.PowerOnVM(ctx, pool.Spec.VCD.VApp, n.ProviderID); err != nil {
					return fmt.Errorf("power on %s: %w", n.Name, err)
				}
			}
			n.Resize.Phase = resizePhaseBooting
			n.Resize.Message = "already carries the target shape; waiting for the Node to become Ready"
			n.Message = n.Resize.Message
			return nil
		}
		if vm.PoweredOn {
			if err := r.Cloud.PowerOffVM(ctx, pool.Spec.VCD.VApp, n.ProviderID); err != nil {
				return fmt.Errorf("stop %s: %w", n.Name, err)
			}
		}
		n.Resize.Phase = resizePhaseStopped
		n.Resize.Message = "drained and powered off; resizing in the next pass"
		n.Message = n.Resize.Message
		return nil

	case resizePhaseStopped:
		// CPU, memory and the system disk are changed in this one power-off
		// window, and the worker is only powered on again once the VM reports
		// the target shape (I-17). vCD resizes a stopped VM only, so a VM that
		// something started again is stopped first instead of failing the slot
		// (I-10).
		vm, err := r.Cloud.GetVM(ctx, pool.Spec.VCD.VApp, n.ProviderID)
		if err != nil {
			return fmt.Errorf("read %s: %w", n.Name, err)
		}
		have := &infrav1.FlavorStatus{CPUs: vm.CPU, MemoryMB: vm.MemoryMB, OSDiskGB: vm.OSDiskGB}
		if compareFlavor(have, target) != flavorEqual {
			if vm.PoweredOn {
				if err := r.Cloud.PowerOffVM(ctx, pool.Spec.VCD.VApp, n.ProviderID); err != nil {
					return fmt.Errorf("stop %s: %w", n.Name, err)
				}
				n.Resize.Message = "powered off again; resizing in the next pass"
				n.Message = n.Resize.Message
				return nil
			}
			resized, err := r.Cloud.ResizeVM(ctx, pool.Spec.VCD.VApp, n.ProviderID, target.CPUs, target.MemoryMB, target.OSDiskGB)
			if err != nil {
				if errors.Is(err, cloud.ErrPoweredOn) {
					if perr := r.Cloud.PowerOffVM(ctx, pool.Spec.VCD.VApp, n.ProviderID); perr != nil {
						return fmt.Errorf("stop %s: %w", n.Name, perr)
					}
					n.Resize.Message = "the VM was running; powered off and resizing in the next pass"
					n.Message = n.Resize.Message
					return nil
				}
				return fmt.Errorf("resize %s: %w", n.Name, err)
			}
			vm = resized
			have = &infrav1.FlavorStatus{CPUs: vm.CPU, MemoryMB: vm.MemoryMB, OSDiskGB: vm.OSDiskGB}
			if compareFlavor(have, target) != flavorEqual {
				// Do not boot a worker with the wrong shape: a resize that
				// only half applied would otherwise look like a finished one.
				return fmt.Errorf("worker %s reports %d vCPU / %d MB / %d GB after the resize, want %d / %d / %d; it stays powered off",
					n.Name, have.CPUs, have.MemoryMB, have.OSDiskGB, target.CPUs, target.MemoryMB, target.OSDiskGB)
			}
		}
		if !vm.PoweredOn {
			if err := r.Cloud.PowerOnVM(ctx, pool.Spec.VCD.VApp, n.ProviderID); err != nil {
				return fmt.Errorf("power on %s: %w", n.Name, err)
			}
		}
		n.Flavor = have
		n.Resize.Phase = resizePhaseBooting
		n.Resize.Message = "resized and powered on; waiting for the Node to become Ready"
		n.Message = n.Resize.Message
		r.eventf(pool, corev1.EventTypeNormal, "WorkerResized",
			fmt.Sprintf("worker %s resized to %d vCPU / %d MB", n.Name, target.CPUs, target.MemoryMB))
		return nil

	case resizePhaseBooting:
		nodes, err := r.Kube.ListNodes(ctx)
		if err != nil {
			return err
		}
		var info *kube.NodeInfo
		for i := range nodes {
			if strings.EqualFold(nodes[i].Name, kubeName) {
				info = &nodes[i]
				break
			}
		}
		if info == nil || !info.Ready {
			if n.Resize.StartedAt != nil && time.Since(n.Resize.StartedAt.Time) > rolloutReadyTimeout(pool) {
				return fmt.Errorf("worker %s did not become Ready within %s after the resize",
					n.Name, rolloutReadyTimeout(pool))
			}
			n.Resize.Message = "waiting for the Node to become Ready"
			return nil
		}
		if compareFlavor(n.Flavor, target) != flavorEqual {
			return fmt.Errorf("worker %s reports %+v after the resize, want %+v", n.Name, n.Flavor, target)
		}
		return r.finishResize(ctx, pool, n, target, kubeName)

	case resizePhaseFailed:
		// Only a worker that already carries the target shape can be closed out
		// here; anything else keeps its failure for the operator.
		if compareFlavor(n.Flavor, target) != flavorEqual {
			return nil
		}
		nodes, err := r.Kube.ListNodes(ctx)
		if err != nil {
			return err
		}
		for i := range nodes {
			if strings.EqualFold(nodes[i].Name, kubeName) && nodes[i].Ready {
				n.Resize.Message = "retrying the closing steps of the resize"
				return r.finishResize(ctx, pool, n, target, kubeName)
			}
		}
		return nil
	}
	return fmt.Errorf("worker %s: unknown rollout phase %q", n.Name, n.Resize.Phase)
}

// finishResize puts a resized worker back into service. The worker is uncordoned
// first, so a pool update that the edge keeps rejecting does not hold a healthy
// node out of the cluster: the slot stays in the resize state and the next pass
// retries the (idempotent) pool add.
func (r *WorkerPoolReconciler) finishResize(ctx context.Context, pool *infrav1.K8sWorkerPool, n *infrav1.WorkerNodeStatus, target *infrav1.FlavorStatus, kubeName string) error {
	if err := r.Kube.Uncordon(ctx, kubeName); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("uncordon %s: %w", kubeName, err)
	}
	if err := r.Cloud.AddPoolMember(ctx, toPoolRef(pool), n.IP); err != nil && !errors.Is(err, cloud.ErrPoolNotConfigured) {
		msg := fmt.Sprintf("resized and in service; waiting for the ingress pool: %v", err)
		if n.Resize.Message != msg {
			log.FromContext(ctx).Error(err, "add the worker back to the ingress pool", "worker", n.Name, "ip", n.IP)
			r.eventf(pool, corev1.EventTypeWarning, "IngressPoolFailed",
				fmt.Sprintf("worker %s (%s) is back in service but not in the ingress pool: %v", n.Name, n.IP, err))
		}
		n.Resize.Message = msg
		n.Message = msg
		return nil
	}
	n.Resize = nil
	n.Phase = infrav1.NodePhaseReady
	n.Message = ""
	r.eventf(pool, corev1.EventTypeNormal, "WorkerResizeComplete",
		fmt.Sprintf("worker %s is back in service with %d vCPU / %d MB", n.Name, target.CPUs, target.MemoryMB))
	return nil
}

// resizeAllowed lists the workers opted into a Manual rollout through the
// k8s-nodescale.github.io/resize=allow annotation.
func (r *WorkerPoolReconciler) resizeAllowed(ctx context.Context) (map[string]bool, error) {
	nodes, err := r.Kube.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if strings.EqualFold(n.Annotations[infrav1.ResizeAnnotation], "allow") {
			out[strings.ToLower(n.Name)] = true
		}
	}
	return out, nil
}
