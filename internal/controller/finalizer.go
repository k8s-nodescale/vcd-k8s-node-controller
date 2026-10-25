package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/ipam"
)

// ensureFinalizer puts the cleanup finalizer on a live pool. The patch
// updates the object in place, so the pass continues with the current
// resourceVersion instead of spending a requeue on the write.
func (r *WorkerPoolReconciler) ensureFinalizer(ctx context.Context, pool *infrav1.K8sWorkerPool) error {
	if controllerutil.ContainsFinalizer(pool, infrav1.FinalizerName) {
		return nil
	}
	base := pool.DeepCopy()
	controllerutil.AddFinalizer(pool, infrav1.FinalizerName)
	if err := r.Patch(ctx, pool, client.MergeFrom(base)); err != nil {
		return err
	}
	log.FromContext(ctx).Info("cleanup finalizer added", "pool", pool.Name, "finalizer", infrav1.FinalizerName)
	return nil
}

// reconcilePoolDeletion removes the workers of a pool whose object is being
// deleted, then releases the finalizer. Without this a "kubectl delete
// k8sworkerpool" would drop the CR and orphan every VM, Node object, ingress
// pool member and bootstrap token it owned.
//
// The observed state is rebuilt from vCD rather than trusted from status: a
// pool deleted right after creation, or one whose status write never landed,
// must still be cleaned up. One worker is advanced per pass, exactly like a
// scale-down, so a long drain does not block the work queue.
func (r *WorkerPoolReconciler) reconcilePoolDeletion(ctx context.Context, pool *infrav1.K8sWorkerPool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(pool, infrav1.FinalizerName) {
		// Nothing to clean up: the object was created before the finalizer
		// existed, or another writer removed it already.
		return ctrl.Result{}, nil
	}

	all, err := r.Cloud.ListWorkerVMs(ctx, pool.Spec.VCD.VApp, nil)
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "CloudError", err.Error())
		return ctrl.Result{}, err
	}
	alloc, err := ipam.New(toIPAMPlan(pool.Spec.IPPlan, pool.Spec.VCD), toIPAMLedger(pool.Status.Ledger))
	if err != nil {
		// The ledger cannot be rebuilt, but the VMs can still be removed. A
		// bad IP plan must not pin the finalizer forever.
		logger.Error(err, "cannot rebuild the address ledger during pool deletion", "pool", pool.Name)
		return ctrl.Result{}, err
	}
	nodes, _, err := mergeNodes(pool, ownedWorkerVMs(pool, all), alloc)
	if err != nil {
		r.setCondition(pool, metav1.ConditionFalse, "InvalidIPPlan", err.Error())
		return ctrl.Result{RequeueAfter: requeueInvalidSpec}, nil
	}
	pool.Status.Nodes = nodes
	pool.Status.ObservedReplicas = int32(len(nodes))
	pool.Status.Ledger = toStatusLedger(alloc.Ledger())

	if len(nodes) == 0 {
		return r.releasePool(ctx, pool)
	}

	msg := fmt.Sprintf("deleting the pool: %d worker(s) left; each is drained and removed before the object disappears", len(nodes))
	if c := meta.FindStatusCondition(pool.Status.Conditions, "Ready"); c == nil || c.Reason != "PoolDeleting" || c.Message != msg {
		r.eventf(pool, corev1.EventTypeNormal, "WorkerPoolDeleting", msg)
	}
	r.setCondition(pool, metav1.ConditionFalse, "PoolDeleting", msg)

	// Honour the per-node force-delete annotation as well as the spec flag: a
	// worker whose local volumes were already accepted as lost must not block
	// the pool deletion.
	force := pool.Spec.ScaleDown.Force
	if requests, err := r.deletionRequests(ctx, pool); err == nil {
		for _, cand := range requests {
			if strings.EqualFold(cand.name, nodes[0].Name) && cand.force {
				force = true
			}
		}
	}
	req := deletionRequest{name: nodes[0].Name, force: force, finalizer: true}
	_, res, err := r.deleteWorkerStep(ctx, pool, alloc, 0, req)
	return res, err
}

// releasePool removes the finalizer once no worker is left. The object is
// deleted by the API server immediately afterwards, so a failed status write
// is not an error.
func (r *WorkerPoolReconciler) releasePool(ctx context.Context, pool *infrav1.K8sWorkerPool) (ctrl.Result, error) {
	r.eventf(pool, corev1.EventTypeNormal, "WorkerPoolCleaned",
		"every worker of the pool is gone; releasing the object")
	base := pool.DeepCopy()
	controllerutil.RemoveFinalizer(pool, infrav1.FinalizerName)
	if err := r.Patch(ctx, pool, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
