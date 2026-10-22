package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/werton13/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/werton13/vcd-k8s-node-controller/internal/cloud"
	"github.com/werton13/vcd-k8s-node-controller/internal/ipam"
	"github.com/werton13/vcd-k8s-node-controller/internal/kube"
)

const (
	// DeleteWorkerAnnotation marks a Node for removal on the next reconcile:
	//
	//	kubectl annotate node <worker> infra.example.com/delete-worker=true
	//
	// The node is drained and its VM deleted; because spec.replicas is left
	// alone, the pool then creates a replacement (rolling replacement).
	// Use spec.scaleDown.deleteWorkers to remove a worker for good.
	DeleteWorkerAnnotation = "infra.example.com/delete-worker"
	// ForceDeleteAnnotation skips the node-local volume guard for one node.
	ForceDeleteAnnotation = "infra.example.com/force-delete"

	defaultDrainTimeout = 10 * time.Minute
	deleteStepRequeue   = 5 * time.Second
	drainRetryRequeue   = 10 * time.Second
	volumeWaitTimeout   = 2 * time.Minute
)

// deletionRequest is one worker the user asked to remove.
type deletionRequest struct {
	name   string
	inSpec bool // listed in spec.scaleDown.deleteWorkers
	force  bool
}

// reconcileDeletions processes named deletion requests before the replica
// arithmetic, so deleting a specific worker is never mistaken for a scale-up.
func (r *WorkerPoolReconciler) reconcileDeletions(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator) (bool, ctrl.Result, error) {
	requests, err := r.deletionRequests(ctx, pool)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	requests = appendInFlightDeletions(pool, requests)
	if len(requests) == 0 {
		return false, ctrl.Result{}, nil
	}
	return r.runDeletions(ctx, pool, alloc, requests)
}

// deletionRequests merges the declarative list with the per-node annotation.
func (r *WorkerPoolReconciler) deletionRequests(ctx context.Context, pool *infrav1.K8sWorkerPool) ([]deletionRequest, error) {
	var out []deletionRequest
	seen := map[string]bool{}
	for _, name := range pool.Spec.ScaleDown.DeleteWorkers {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, deletionRequest{name: name, inSpec: true})
	}

	nodes, err := r.Kube.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if !strings.EqualFold(node.Annotations[DeleteWorkerAnnotation], "true") {
			continue
		}
		key := strings.ToLower(node.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, deletionRequest{
			name:  node.Name,
			force: strings.EqualFold(node.Annotations[ForceDeleteAnnotation], "true"),
		})
	}
	return out, nil
}

// appendInFlightDeletions keeps a removal that is already under way alive.
// A count-based scale-down is recomputed from spec.replicas on every pass, and
// once the worker is Draining it is no longer a scale-down candidate, so
// without this the next pass would start a second deletion instead of finishing
// the first one.
func appendInFlightDeletions(pool *infrav1.K8sWorkerPool, requests []deletionRequest) []deletionRequest {
	seen := make(map[string]bool, len(requests))
	for _, req := range requests {
		seen[strings.ToLower(req.name)] = true
	}
	for _, node := range pool.Status.Nodes {
		if node.DeleteStartedAt == nil {
			continue
		}
		if node.Phase != infrav1.NodePhaseDraining && node.Phase != infrav1.NodePhaseDeleting {
			continue
		}
		key := strings.ToLower(node.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		requests = append(requests, deletionRequest{name: node.Name})
	}
	return requests
}

// runDeletions advances each requested worker by at most one step and then
// returns. Draining is a minutes-long operation; one step per pass keeps the
// work queue free and makes progress visible in status.
func (r *WorkerPoolReconciler) runDeletions(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, requests []deletionRequest) (bool, ctrl.Result, error) {
	for _, req := range requests {
		idx := nodeIndex(pool.Status.Nodes, req.name)
		if idx < 0 {
			// The worker is already gone (its slot was written off, or the VM
			// vanished): satisfy the request so it does not sit in the spec
			// forever, and line spec.replicas up with what is left. Nothing is
			// created to replace it.
			if req.inSpec {
				pruneDeleteWorker(pool, req.name)
				if pool.Spec.Replicas > int32(len(pool.Status.Nodes)) {
					pool.Spec.Replicas = int32(len(pool.Status.Nodes))
				}
				r.eventf(pool, corev1.EventTypeNormal, "WorkerDeleteRequestPruned",
					fmt.Sprintf("worker %s is already gone; the request was dropped and spec.replicas aligned to %d", req.name, pool.Spec.Replicas))
			}
			continue
		}
		finished, res, err := r.deleteWorkerStep(ctx, pool, alloc, idx, req)
		if err != nil || !finished {
			return true, res, err
		}
	}
	return true, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil
}

// deleteWorkerStep runs one step of cordon -> ingress removal -> drain ->
// detach wait -> power off -> node delete -> VM delete -> IP release.
func (r *WorkerPoolReconciler) deleteWorkerStep(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, idx int, req deletionRequest) (bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	node := &pool.Status.Nodes[idx]

	// A slot whose VM is already gone needs no drain and no cloud call: the
	// request is satisfied by writing the slot off and releasing its address.
	// Without this a named deletion of a vanished worker would try to power off
	// an empty VM ID and park the pool in ScaleDownFailed.
	if node.Phase == infrav1.NodePhaseMissing {
		if node.IP != "" {
			_ = alloc.Free(node.IP)
		}
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		pool.Status.Nodes = append(pool.Status.Nodes[:idx], pool.Status.Nodes[idx+1:]...)
		pool.Status.ObservedReplicas--
		pruneDeleteWorker(pool, node.Name)
		if pool.Spec.Replicas > int32(len(pool.Status.Nodes)) {
			pool.Spec.Replicas = int32(len(pool.Status.Nodes))
		}
		r.eventf(pool, corev1.EventTypeNormal, "WorkerWrittenOff",
			fmt.Sprintf("missing worker %s (%s) written off; address released", node.Name, node.IP))
		return true, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil
	}

	// A failed resize left the worker cordoned and out of service; the removal
	// pipeline takes it from here and the rollout must stop tracking it.
	node.Resize = nil

	// Every Kubernetes call below uses kubeName, never node.Name: name is the
	// vCD VM name (KION-WEB-DEV-wrk-03), while kubelet registers the Node
	// object in lower case. With the VM name the API answers 404, Cordon and
	// DeleteNode swallow it and Drain selects no pods, so the worker was
	// deleted with its guest still running: no cordon, no drain, no PDB, and
	// a NotReady Node object left behind forever.
	kubeName, err := r.kubeNodeName(ctx, node)
	if err != nil {
		return false, ctrl.Result{}, err
	}

	if node.Phase == infrav1.NodePhaseFailed {
		// The request is still standing, so the operator wants this worker gone:
		// start a fresh drainTimeout budget. A PDB that never allows a
		// disruption makes this retry every budget, which is slower than a
		// tight loop and still leaves the reason in the message.
		now := metav1.Now()
		node.DeleteStartedAt = &now
		node.Phase = infrav1.NodePhaseDraining
		node.Message = "retrying the drain with a fresh budget"
		r.eventf(pool, corev1.EventTypeNormal, "WorkerDeletionRetried",
			fmt.Sprintf("retrying drain of %s with drainTimeout=%s", node.Name, budgetFor(pool)))
	}

	after := int32(len(pool.Status.Nodes)) - 1
	if minReplicas := pool.Spec.ScaleDown.MinReplicas; minReplicas > 0 && after < minReplicas {
		msg := fmt.Sprintf("refusing to delete %s: %d worker(s) would fall below scaleDown.minReplicas=%d",
			node.Name, after, minReplicas)
		node.Message = msg
		r.setCondition(pool, metav1.ConditionFalse, "MinReplicasProtected", msg)
		r.eventf(pool, corev1.EventTypeWarning, "MinReplicasProtected", msg)
		return true, ctrl.Result{RequeueAfter: requeueConverged}, nil
	}
	// Emptying the pool is a deliberate owner decision: both conditions must
	// be explicit, and two refusals survive the flag (the worker this pass runs
	// on, and node-local volumes, checked below).
	guardName := kubeName
	if guardName == "" {
		guardName = node.Name
	}
	if reason, msg := r.checkLastWorker(ctx, pool, guardName, int(after)); reason != "" {
		node.Message = msg
		r.setCondition(pool, metav1.ConditionFalse, reason, msg)
		r.eventf(pool, corev1.EventTypeWarning, reason, msg)
		return true, ctrl.Result{RequeueAfter: requeueConverged}, nil
	}

	budget := budgetFor(pool)

	if node.DeleteStartedAt == nil {
		if !pool.Spec.ScaleDown.Force && !req.force && kubeName != "" {
			local, err := r.Kube.NodeLocalVolumes(ctx, kubeName)
			if err != nil {
				return false, ctrl.Result{}, err
			}
			if len(local) > 0 {
				msg := fmt.Sprintf("refusing to delete %s: node-local volumes hold data (%s); set spec.scaleDown.force or annotate the node with %s=true",
					node.Name, strings.Join(local, ", "), ForceDeleteAnnotation)
				node.Message = msg
				r.setCondition(pool, metav1.ConditionFalse, "VolumeDataAtRisk", msg)
				r.eventf(pool, corev1.EventTypeWarning, "VolumeDataAtRisk", msg)
				return true, ctrl.Result{RequeueAfter: requeueConverged}, nil
			}
		}

		now := metav1.Now()
		node.DeleteStartedAt = &now
		node.Phase = infrav1.NodePhaseDraining
		node.Message = "cordoned; removing from the ingress pool"

		if kubeName != "" {
			if err := r.Kube.Cordon(ctx, kubeName); err != nil && !apierrors.IsNotFound(err) {
				node.Message = "cordon failed: " + err.Error()
				return false, ctrl.Result{}, err
			}
		}
		if err := r.removeFromIngressPool(ctx, pool, node.IP); err != nil {
			// Not fatal: the pool's health monitor takes the address out once
			// the node stops answering. The event keeps the gap visible.
			logger.Error(err, "could not remove worker from the ingress pool", "worker", node.Name)
			node.Message = "ingress pool member not removed: " + err.Error()
			r.eventf(pool, corev1.EventTypeWarning, "IngressPoolMemberNotRemoved",
				fmt.Sprintf("worker %s: %v", node.Name, err))
		}
		r.eventf(pool, corev1.EventTypeNormal, "WorkerDeletionStarted",
			fmt.Sprintf("draining %s (drainTimeout=%s)", node.Name, budget))
		return false, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil
	}

	deadline := node.DeleteStartedAt.Add(budget)
	switch node.Phase {
	case infrav1.NodePhaseDraining:
		if time.Now().After(deadline) {
			node.Phase = infrav1.NodePhaseFailed
			msg := fmt.Sprintf("drain did not finish within %s: %s", budget, node.Message)
			node.Message = msg
			r.setCondition(pool, metav1.ConditionFalse, "ScaleDownFailed", msg)
			r.eventf(pool, corev1.EventTypeWarning, "ScaleDownFailed", msg)
			return true, ctrl.Result{RequeueAfter: requeueProgressing}, nil
		}
		if kubeName == "" {
			// No Node object: the worker never joined, or its registration is
			// gone already. Nothing to drain, go straight to the deletion.
			node.Phase = infrav1.NodePhaseDeleting
			node.Message = "no Node object to drain; waiting for volumes to detach"
			return false, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil
		}
		err := r.Kube.Drain(ctx, kubeName, time.Until(deadline))
		switch {
		case err == nil, apierrors.IsNotFound(err):
			node.Phase = infrav1.NodePhaseDeleting
			node.Message = "waiting for volumes to detach"
			return false, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil
		case errors.Is(err, kube.ErrDrainIncomplete):
			node.Message = err.Error()
			return false, ctrl.Result{RequeueAfter: drainRetryRequeue}, nil
		default:
			node.Message = "drain failed: " + err.Error()
			return false, ctrl.Result{}, err
		}

	case infrav1.NodePhaseDeleting:
		if kubeName != "" {
			if err := r.Kube.WaitVolumesDetached(ctx, kubeName, volumeWaitTimeout); err != nil {
				if errors.Is(err, kube.ErrVolumesAttached) {
					node.Message = err.Error()
					return false, ctrl.Result{RequeueAfter: drainRetryRequeue}, nil
				}
				return false, ctrl.Result{}, err
			}
		}
		// Stop the guest before the Node object is deleted. A Node deleted
		// while kubelet still runs is re-registered within seconds, so the
		// worker comes back as a NotReady corpse that nothing owns and whose
		// pods are never garbage collected.
		if err := r.Cloud.PowerOffVM(ctx, pool.Spec.VCD.VApp, node.VMID); err != nil {
			node.Message = "VM power-off failed: " + err.Error()
			return false, ctrl.Result{}, err
		}
		if kubeName != "" {
			if err := r.Kube.DeleteNode(ctx, kubeName); err != nil && !apierrors.IsNotFound(err) {
				node.Message = "node deletion failed: " + err.Error()
				return false, ctrl.Result{}, err
			}
		}
		if err := r.Cloud.DeleteVM(ctx, pool.Spec.VCD.VApp, node.VMID); err != nil {
			node.Message = "VM deletion failed: " + err.Error()
			return false, ctrl.Result{}, err
		}

		name, ip := node.Name, node.IP
		if ip != "" {
			_ = alloc.Free(ip)
		}
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		pool.Status.Nodes = append(pool.Status.Nodes[:idx], pool.Status.Nodes[idx+1:]...)
		pool.Status.ObservedReplicas--
		if req.inSpec {
			// The request asked for this worker to be gone, not replaced:
			// drop the entry and keep replicas consistent with what is left.
			pruneDeleteWorker(pool, name)
			if pool.Spec.Replicas > 0 {
				pool.Spec.Replicas--
			}
		}
		msg := fmt.Sprintf("worker %s (%s) deleted", name, ip)
		r.eventf(pool, corev1.EventTypeNormal, "WorkerDeleted", msg)
		r.setCondition(pool, metav1.ConditionFalse, "Scaling", msg)
		return true, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil

	default:
		// Status was written by an older build, or the phase was lost: resume
		// at draining instead of restarting the whole sequence.
		node.Phase = infrav1.NodePhaseDraining
		return false, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil
	}
}

// scaleDown removes n workers, highest ordinal first. A missing slot is
// written off before a live worker is considered, and a worker adopted in this
// pass is never a candidate: an outside terraform apply must not turn into
// "the controller removed a different worker".
func (r *WorkerPoolReconciler) scaleDown(ctx context.Context, pool *infrav1.K8sWorkerPool, alloc *ipam.Allocator, n int32, adopted []string) (bool, ctrl.Result, error) {
	skip := make(map[string]bool, len(adopted))
	for _, name := range adopted {
		skip[strings.ToLower(name)] = true
	}

	// A slot whose VM disappeared costs nothing to write off: no drain, no VM
	// to delete, only the ledger entry and the status row. Doing this first is
	// what keeps a count-based scale-in from removing a live worker while the
	// corpse of a vanished one still holds an address.
	remaining := n
	for i := 0; i < len(pool.Status.Nodes) && remaining > 0; {
		node := pool.Status.Nodes[i]
		if node.Phase != infrav1.NodePhaseMissing {
			i++
			continue
		}
		if node.IP != "" {
			_ = alloc.Free(node.IP)
		}
		pool.Status.Ledger = toStatusLedger(alloc.Ledger())
		pool.Status.Nodes = append(pool.Status.Nodes[:i], pool.Status.Nodes[i+1:]...)
		pool.Status.ObservedReplicas--
		pruneDeleteWorker(pool, node.Name)
		r.eventf(pool, corev1.EventTypeNormal, "WorkerWrittenOff",
			fmt.Sprintf("missing worker %s (%s) written off; address released", node.Name, node.IP))
		remaining--
	}
	if remaining == 0 {
		return true, ctrl.Result{RequeueAfter: deleteStepRequeue}, nil
	}

	candidates := make([]infrav1.WorkerNodeStatus, 0, len(pool.Status.Nodes))
	for _, node := range pool.Status.Nodes {
		switch node.Phase {
		case infrav1.NodePhaseDraining, infrav1.NodePhaseDeleting, infrav1.NodePhaseFailed,
			infrav1.NodePhaseMissing, infrav1.NodePhaseResizing:
			continue
		}
		if skip[strings.ToLower(node.Name)] {
			continue
		}
		candidates = append(candidates, node)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return workerOrdinal(candidates[i].Name) > workerOrdinal(candidates[j].Name)
	})

	// A worker left in Failed still occupies an address and counts as observed.
	// Deleting a healthy worker to satisfy the count would hide it, so the pool
	// stops until the failed worker is resolved or its request is withdrawn.
	for _, node := range pool.Status.Nodes {
		if node.Phase != infrav1.NodePhaseFailed {
			continue
		}
		msg := fmt.Sprintf("scale-down paused: %s is Failed (%s); retry or withdraw its deletion request first",
			node.Name, node.Message)
		r.setCondition(pool, metav1.ConditionFalse, "ScaleDownFailed", msg)
		r.eventf(pool, corev1.EventTypeWarning, "ScaleDownFailed", msg)
		return true, ctrl.Result{RequeueAfter: requeueConverged}, nil
	}

	minReplicas := pool.Spec.ScaleDown.MinReplicas
	guard := false
	var requests []deletionRequest
	for _, node := range candidates {
		if int32(len(requests)) >= remaining {
			break
		}
		left := int32(len(pool.Status.Nodes) - len(requests) - 1)
		if minReplicas > 0 && left < minReplicas {
			msg := fmt.Sprintf("scale-down stopped at %d worker(s): scaleDown.minReplicas=%d", left+1, minReplicas)
			r.setCondition(pool, metav1.ConditionFalse, "MinReplicasProtected", msg)
			r.eventf(pool, corev1.EventTypeWarning, "MinReplicasProtected", msg)
			guard = true
			break
		}
		if reason, msg := r.checkLastWorker(ctx, pool, strings.ToLower(node.Name), int(left)); reason != "" {
			r.setCondition(pool, metav1.ConditionFalse, reason, msg)
			r.eventf(pool, corev1.EventTypeWarning, reason, msg)
			guard = true
			break
		}
		requests = append(requests, deletionRequest{name: node.Name})
	}
	if len(requests) == 0 {
		if guard {
			// A policy refused the removal and its condition must not be
			// overwritten by the readiness pass in this same reconcile.
			return true, ctrl.Result{RequeueAfter: requeueConverged}, nil
		}
		// Every candidate is already being removed or was adopted in this
		// pass: let the readiness pass report progress.
		return false, ctrl.Result{}, nil
	}
	return r.runDeletions(ctx, pool, alloc, requests)
}

// budgetFor returns the drain budget the pool actually uses.
func budgetFor(pool *infrav1.K8sWorkerPool) time.Duration {
	if budget := pool.Spec.ScaleDown.DrainTimeout.Duration; budget > 0 {
		return budget
	}
	return defaultDrainTimeout
}

// removeFromIngressPool takes the worker address out of the ingress pool so no
// new traffic is sent to a node that is about to disappear.
func (r *WorkerPoolReconciler) removeFromIngressPool(ctx context.Context, pool *infrav1.K8sWorkerPool, ip string) error {
	if strings.TrimSpace(pool.Spec.IngressPool.Name) == "" || ip == "" {
		return nil
	}
	return r.Cloud.RemovePoolMember(ctx, toPoolRef(pool), ip)
}

func toPoolRef(pool *infrav1.K8sWorkerPool) cloud.PoolRef {
	return cloud.PoolRef{
		Type: pool.Spec.IngressPool.Type,
		Name: pool.Spec.IngressPool.Name,
		Port: pool.Spec.IngressPool.Port,
		Edge: pool.Spec.IngressPool.Edge,
	}
}

// eventf records a worker-pool event when a recorder is configured.
func (r *WorkerPoolReconciler) eventf(pool *infrav1.K8sWorkerPool, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(pool, eventType, reason, message)
}

// kubeNodeName returns the Kubernetes Node object name of a worker, resolving
// it against the live Node list when status does not carry it yet (status
// written by an older build, or a worker adopted in this pass). An empty
// result means the worker has no Node object, which the removal path treats as
// "nothing to drain": the VM is stopped and removed only.
func (r *WorkerPoolReconciler) kubeNodeName(ctx context.Context, node *infrav1.WorkerNodeStatus) (string, error) {
	if node.NodeName != "" {
		return node.NodeName, nil
	}
	nodes, err := r.Kube.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if strings.EqualFold(n.Name, node.Name) {
			node.NodeName = n.Name
			return n.Name, nil
		}
	}
	return "", nil
}

func nodeIndex(nodes []infrav1.WorkerNodeStatus, name string) int {
	for i := range nodes {
		if strings.EqualFold(nodes[i].Name, name) {
			return i
		}
	}
	return -1
}

func pruneDeleteWorker(pool *infrav1.K8sWorkerPool, name string) {
	kept := pool.Spec.ScaleDown.DeleteWorkers[:0]
	for _, entry := range pool.Spec.ScaleDown.DeleteWorkers {
		if strings.EqualFold(strings.TrimSpace(entry), name) {
			continue
		}
		kept = append(kept, entry)
	}
	pool.Spec.ScaleDown.DeleteWorkers = kept
}

// workerOrdinal returns the numeric suffix of a worker name so scale-down can
// remove the newest workers first. Names that do not follow the
// <prefix>-wrk-NN convention sort last.
func workerOrdinal(name string) int {
	if idx := strings.LastIndex(name, "-"); idx >= 0 && idx+1 < len(name) {
		if n, err := strconv.Atoi(name[idx+1:]); err == nil {
			return n
		}
	}
	return -1
}
