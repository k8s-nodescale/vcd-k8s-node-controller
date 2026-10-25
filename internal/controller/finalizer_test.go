package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

func TestReconcileAddsCleanupFinalizer(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if !controllerutil.ContainsFinalizer(got, infrav1.FinalizerName) {
		t.Fatalf("finalizers = %v, want %s", got.Finalizers, infrav1.FinalizerName)
	}
	// The finalizer write must not replace the rest of the pass: the pool still
	// converges in the same reconcile.
	if len(fc.created) != 1 {
		t.Fatalf("created %d VM(s), want the pool to keep converging", len(fc.created))
	}
}

func TestPoolDeletionRemovesEveryWorkerAndReleasesTheFinalizer(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7"), workerVM(2, "192.168.100.8")}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("adoption pass: %v", err)
	}
	if got := getPool(t, r, pool); !controllerutil.ContainsFinalizer(got, infrav1.FinalizerName) {
		t.Fatalf("finalizers = %v, want %s", got.Finalizers, infrav1.FinalizerName)
	}

	if err := r.Delete(ctx, getPool(t, r, pool)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for i := 0; i < 12; i++ {
		_, err := r.Reconcile(ctx, requestFor(pool))
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	var gone infrav1.K8sWorkerPool
	err := r.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the pool object still exists after deletion (err=%v, finalizers=%v, nodes=%v)",
			err, gone.Finalizers, nodeNames(&gone))
	}
	if len(fc.vms) != 0 {
		t.Fatalf("VMs left behind: %v", fc.vms)
	}
	if len(fk.deletedNodes) != 2 {
		t.Fatalf("deleted nodes = %v, want both workers", fk.deletedNodes)
	}
	if len(fk.drained) != 2 {
		t.Fatalf("drained workers = %v, want both", fk.drained)
	}
}

func TestPoolDeletionIgnoresScaleDownPolicy(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	// The strictest scale-down policy: the last worker may not be removed.
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{
		MinReplicas:     1,
		DrainTimeout:    metav1.Duration{Duration: time.Minute},
		AllowLastWorker: false,
	}
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("adoption pass: %v", err)
	}
	if err := r.Delete(ctx, getPool(t, r, pool)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for i := 0; i < 8; i++ {
		if _, err := r.Reconcile(ctx, requestFor(pool)); apierrors.IsNotFound(err) {
			break
		} else if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	var gone infrav1.K8sWorkerPool
	if err := r.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the scale-down policy kept a deleted pool alive: finalizers=%v nodes=%v message=%q",
			gone.Finalizers, nodeNames(&gone), readyMessage(&gone))
	}
	if len(fc.vms) != 0 {
		t.Fatalf("VM left behind: %v", fc.vms)
	}
}

func TestPoolDeletionWaitsForTheControllerToMoveOffTheWorker(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("adoption pass: %v", err)
	}

	// The controller runs on the worker it is asked to delete.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-0", Namespace: pool.Namespace},
		Spec:       corev1.PodSpec{NodeName: "pool-wrk-01"},
	}
	if err := r.Create(ctx, pod); err != nil {
		t.Fatalf("create controller pod: %v", err)
	}
	r.PodName, r.PodNamespace = pod.Name, pod.Namespace

	if err := r.Delete(ctx, getPool(t, r, pool)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	got := getPool(t, r, pool)
	if !controllerutil.ContainsFinalizer(got, infrav1.FinalizerName) {
		t.Fatalf("the finalizer was released while the controller still runs on the worker")
	}
	if len(fc.vms) != 1 {
		t.Fatalf("the worker was removed anyway: VMs = %v", fc.vms)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "WorkerHostsController" {
		t.Fatalf("condition = %#v, want reason WorkerHostsController", cond)
	}
	if !strings.Contains(cond.Message, "runs on that worker") {
		t.Fatalf("condition message = %q", cond.Message)
	}
}

func readyMessage(pool *infrav1.K8sWorkerPool) string {
	if c := meta.FindStatusCondition(pool.Status.Conditions, "Ready"); c != nil {
		return c.Reason + ": " + c.Message
	}
	return ""
}
