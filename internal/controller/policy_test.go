package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

func reconcileOnce(t *testing.T, r *WorkerPoolReconciler, pool *infrav1.K8sWorkerPool) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func updateSpec(t *testing.T, r *WorkerPoolReconciler, pool *infrav1.K8sWorkerPool, mutate func(*infrav1.K8sWorkerPool)) {
	t.Helper()
	cur := getPool(t, r, pool)
	mutate(cur)
	if err := r.Update(context.Background(), cur); err != nil {
		t.Fatalf("update pool: %v", err)
	}
}

func nodeByName(pool *infrav1.K8sWorkerPool, name string) *infrav1.WorkerNodeStatus {
	for i := range pool.Status.Nodes {
		if pool.Status.Nodes[i].Name == name {
			return &pool.Status.Nodes[i]
		}
	}
	return nil
}

// A VM deleted outside the controller is a lost slot, not a scale-up: Notify
// keeps the address and the name, reports it, and creates nothing.
func TestNotifyKeepsSlotOfVanishedWorker(t *testing.T) {
	pool := testPool(2)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)

	// Somebody deletes wrk-02 in vCD.
	fc.vms = fc.vms[:1]
	fk.nodes = readyNodes("pool-wrk-01")
	reconcileOnce(t, r, pool)

	if len(fc.created) != 2 {
		t.Fatalf("created %d VMs after the loss, want 2 (Notify must not provision a replacement)", len(fc.created))
	}
	got := getPool(t, r, pool)
	if got.Status.ObservedReplicas != 2 || len(got.Status.Nodes) != 2 {
		t.Fatalf("observed=%d nodes=%d, want the slot to stay: 2/2", got.Status.ObservedReplicas, len(got.Status.Nodes))
	}
	missing := nodeByName(got, "pool-wrk-02")
	if missing == nil || missing.Phase != infrav1.NodePhaseMissing {
		t.Fatalf("pool-wrk-02 = %#v, want phase Missing", missing)
	}
	if missing.IP != "192.168.100.8" {
		t.Fatalf("missing slot IP = %q, want 192.168.100.8 (reserved)", missing.IP)
	}
	if len(got.Status.Ledger.Used) != 2 {
		t.Fatalf("ledger used = %v, want both addresses kept", got.Status.Ledger.Used)
	}
	if cond := metaReady(got); cond == nil || cond.Reason != "WorkerMissing" {
		t.Fatalf("Ready condition = %#v, want reason WorkerMissing", cond)
	}
}

// AutoHeal recreates the lost worker into its own slot: same name, same
// address, no successor under a new name and no leaked address.
func TestAutoHealRecreatesTheSameSlot(t *testing.T) {
	pool := testPool(2)
	pool.Spec.Reconciliation = infrav1.ReconciliationPolicy{Policy: infrav1.ReconciliationAutoHeal}
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	fc.vms = fc.vms[:1]
	fk.nodes = readyNodes("pool-wrk-01")
	reconcileOnce(t, r, pool)

	if len(fc.created) != 3 {
		t.Fatalf("created %d VMs, want a third one from AutoHeal", len(fc.created))
	}
	healed := fc.created[2]
	if healed.Name != "pool-wrk-02" || healed.IP != "192.168.100.8" {
		t.Fatalf("AutoHeal created %q at %q, want pool-wrk-02 at 192.168.100.8", healed.Name, healed.IP)
	}
	got := getPool(t, r, pool)
	node := nodeByName(got, "pool-wrk-02")
	if node == nil || node.Phase == infrav1.NodePhaseMissing {
		t.Fatalf("pool-wrk-02 = %#v, want it back in service", node)
	}
	if node.HealAttempts != 1 {
		t.Fatalf("healAttempts = %d, want 1", node.HealAttempts)
	}
	if len(got.Status.Ledger.Used) != 2 {
		t.Fatalf("ledger used = %v, want 2 addresses", got.Status.Ledger.Used)
	}
}

// The AutoHeal budget is finite: a slot that cannot be recreated parks as
// Failed and stays reserved instead of being retried forever.
func TestAutoHealGivesUpAndParksTheSlot(t *testing.T) {
	pool := testPool(2)
	pool.Spec.Reconciliation = infrav1.ReconciliationPolicy{
		Policy:          infrav1.ReconciliationAutoHeal,
		AutoHealRetries: 1,
		AutoHealBackoff: metav1.Duration{Duration: time.Nanosecond},
	}
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	fc.vms = fc.vms[:1]
	fk.nodes = readyNodes("pool-wrk-01")
	fc.failAt = 3 // the heal attempt
	reconcileOnce(t, r, pool)

	node := nodeByName(getPool(t, r, pool), "pool-wrk-02")
	if node == nil || node.HealAttempts != 1 {
		t.Fatalf("pool-wrk-02 = %#v, want one failed attempt", node)
	}

	reconcileOnce(t, r, pool)
	got := getPool(t, r, pool)
	node = nodeByName(got, "pool-wrk-02")
	if node == nil || node.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("pool-wrk-02 = %#v, want phase Failed after the budget", node)
	}
	if len(fc.created) != 2 {
		t.Fatalf("created %d VMs, want no further attempts", len(fc.created))
	}
	if len(got.Status.Ledger.Used) != 2 {
		t.Fatalf("ledger used = %v, the slot must stay reserved", got.Status.Ledger.Used)
	}
}

// Deleting the last worker is refused unless both minReplicas: 0 and
// allowLastWorker: true are set.
func TestLastWorkerNeedsBothExplicitConditions(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 0 })
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	if len(fc.deleted) != 0 {
		t.Fatalf("deleted %v, want a refusal", fc.deleted)
	}
	if cond := metaReady(got); cond == nil || cond.Reason != "LastWorkerProtected" {
		t.Fatalf("Ready condition = %#v, want LastWorkerProtected", cond)
	}
	if nodeByName(got, "pool-wrk-01") == nil {
		t.Fatal("the last worker was removed despite the refusal")
	}

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.Replicas = 0
		p.Spec.ScaleDown.MinReplicas = 0
		p.Spec.ScaleDown.AllowLastWorker = true
	})
	// The removal pipeline is progressive: one step per pass.
	for i := 0; i < 10; i++ {
		reconcileOnce(t, r, pool)
		if len(fc.deleted) > 0 {
			break
		}
	}

	got = getPool(t, r, pool)
	if len(fc.deleted) != 1 {
		t.Fatalf("deleted %v, want the last worker removed once both conditions are set", fc.deleted)
	}
	if got.Status.ObservedReplicas != 0 || len(got.Status.Nodes) != 0 {
		t.Fatalf("observed=%d nodes=%d, want an empty pool", got.Status.ObservedReplicas, len(got.Status.Nodes))
	}
}

// Even with allowLastWorker, a controller that runs on the worker it is about
// to delete must refuse: the pass would die with it.
func TestLastWorkerRefusesTheControllerOwnNode(t *testing.T) {
	pool := testPool(1)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 0, AllowLastWorker: true}
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	own := "pool-wrk-01"
	r.ownNode = &own

	reconcileOnce(t, r, pool)
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 0 })
	reconcileOnce(t, r, pool)

	if len(fc.deleted) != 0 {
		t.Fatalf("deleted %v, want a refusal", fc.deleted)
	}
	if cond := metaReady(getPool(t, r, pool)); cond == nil || cond.Reason != "WorkerHostsController" {
		t.Fatalf("Ready condition = %#v, want WorkerHostsController", cond)
	}
}

// A grow moves a worker through cordon, drain, power off, resize, power on and
// back into service, one step per pass, highest ordinal first.
func TestRolloutGrowsWorkersOneByOne(t *testing.T) {
	pool := testPool(2)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })

	// Pass 1: take the highest ordinal out of service and stop it.
	reconcileOnce(t, r, pool)
	node := nodeByName(getPool(t, r, pool), "pool-wrk-02")
	if node == nil || node.Resize == nil || node.Resize.Phase != resizePhaseStopped {
		t.Fatalf("pool-wrk-02 = %#v, want resize phase %s", node, resizePhaseStopped)
	}

	// Pass 2: resize and power on.
	reconcileOnce(t, r, pool)
	if len(fc.resizes) != 1 || fc.resizes[0] != "vm-pool-wrk-02" {
		t.Fatalf("resized %v, want only vm-pool-wrk-02 (highest ordinal first)", fc.resizes)
	}
	node = nodeByName(getPool(t, r, pool), "pool-wrk-02")
	if node == nil || node.Resize == nil || node.Resize.Phase != resizePhaseBooting {
		t.Fatalf("pool-wrk-02 = %#v, want resize phase %s", node, resizePhaseBooting)
	}

	// Pass 3: the Node is Ready again, so it goes back into service.
	reconcileOnce(t, r, pool)
	node = nodeByName(getPool(t, r, pool), "pool-wrk-02")
	if node == nil || node.Resize != nil || node.Phase != infrav1.NodePhaseReady {
		t.Fatalf("pool-wrk-02 = %#v, want it back in service", node)
	}
	if node.Flavor == nil || node.Flavor.CPUs != 4 {
		t.Fatalf("pool-wrk-02 flavor = %#v, want 4 vCPU", node.Flavor)
	}
	if got := getPool(t, r, pool).Status.Rollout; got.Phase != infrav1.RolloutPhaseProgressing || got.WorkersDone != 1 {
		t.Fatalf("rollout status = %#v, want Progressing 1 done", got)
	}
}

// Shrinking cannot be done in place: the pool reports it instead of silently
// ignoring the spec change.
func TestRolloutReportsUnsupportedShrink(t *testing.T) {
	pool := testPool(2)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 1 })
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	if cond := metaReady(got); cond == nil || cond.Reason != "UnsupportedChange" {
		t.Fatalf("Ready condition = %#v, want UnsupportedChange", cond)
	}
	if len(fc.resizes) != 0 {
		t.Fatalf("resized %v, want no resize attempt for a shrink", fc.resizes)
	}
}

// A named deletion of a worker whose VM already vanished writes the slot off
// and lines spec.replicas up with what is left.
func TestDeleteWorkerOfVanishedSlotAlignsReplicas(t *testing.T) {
	pool := testPool(2)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	fc.vms = fc.vms[:1]
	fk.nodes = readyNodes("pool-wrk-01")
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.DeleteWorkers = []string{"pool-wrk-02"}
	})
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	if got.Spec.Replicas != 1 {
		t.Fatalf("spec.replicas = %d, want 1", got.Spec.Replicas)
	}
	if len(got.Spec.ScaleDown.DeleteWorkers) != 0 {
		t.Fatalf("deleteWorkers = %v, want it pruned", got.Spec.ScaleDown.DeleteWorkers)
	}
	if got.Status.ObservedReplicas != 1 || len(got.Status.Nodes) != 1 {
		t.Fatalf("observed=%d nodes=%d, want 1/1", got.Status.ObservedReplicas, len(got.Status.Nodes))
	}
	if len(got.Status.Ledger.Used) != 1 || got.Status.Ledger.Used[0] != "192.168.100.7" {
		t.Fatalf("ledger used = %v, want only 192.168.100.7", got.Status.Ledger.Used)
	}
}

// A worker that does not come back parks the roll as Paused, and a paused roll
// does not lock the pool: deleteWorkers is the documented escape.
func TestRolloutPausesOnFailureAndLetsDeleteWorkersThrough(t *testing.T) {
	pool := testPool(2)
	fc := &fakeCloud{failResize: true}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 4 })

	// Pass 1 stops the worker, pass 2 fails the resize.
	reconcileOnce(t, r, pool)
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	if got.Status.Rollout.Phase != infrav1.RolloutPhasePaused {
		t.Fatalf("rollout phase = %q, want %s (lastError %q)", got.Status.Rollout.Phase, infrav1.RolloutPhasePaused, got.Status.Rollout.LastError)
	}
	node := nodeByName(got, "pool-wrk-02")
	if node == nil || node.Resize == nil || node.Resize.Phase != resizePhaseFailed {
		t.Fatalf("pool-wrk-02 = %#v, want a failed resize", node)
	}

	fc.failResize = false
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.DeleteWorkers = []string{"pool-wrk-02"}
	})
	for i := 0; i < 8; i++ {
		reconcileOnce(t, r, pool)
		if len(fc.deleted) > 0 {
			break
		}
	}
	if len(fc.deleted) == 0 {
		t.Fatal("deleteWorkers did not get through a paused roll")
	}
	got = getPool(t, r, pool)
	if cond := metaReady(got); cond != nil && cond.Reason == "RolloutInProgress" {
		t.Fatalf("Ready condition = %#v, deleteWorkers must be allowed while the roll is paused", cond)
	}
}

// A step that failed after the VM had already reached the target - the ingress
// pool add at the end of a resize - is retried on the next pass instead of
// parking the slot in Resizing/Failed until the worker is deleted.
func TestRolloutRetriesTheClosingStepsOfAFailedResize(t *testing.T) {
	pool := testPool(1)
	pool.Spec.IngressPool = infrav1.IngressPool{Name: "pool-ingress", Edge: "edge", Port: 30080}
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)

	// Simulate 0.5.0 leaving the slot behind: the VM carries the wanted flavor
	// and the Node is Ready, but the closing pool add failed.
	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-01")
	if n == nil {
		t.Fatal("pool-wrk-01 is missing")
	}
	n.Resize = &infrav1.ResizeStatus{Phase: resizePhaseFailed, Message: "add 192.168.100.7 back to the ingress pool: invalid member name"}
	n.Phase = infrav1.NodePhaseResizing
	if err := r.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	before := len(fc.poolAdds)

	reconcileOnce(t, r, pool)

	got = getPool(t, r, pool)
	n = nodeByName(got, "pool-wrk-01")
	if n == nil || n.Resize != nil {
		t.Fatalf("pool-wrk-01 = %#v, want the failed resize cleared (rollout %+v, ready cond %+v)", n, got.Status.Rollout, metaReady(got))
	}
	if n.Phase != infrav1.NodePhaseReady {
		t.Fatalf("phase = %q, want %s", n.Phase, infrav1.NodePhaseReady)
	}
	if len(fc.poolAdds) != before+1 || fc.poolAdds[len(fc.poolAdds)-1] != "192.168.100.7" {
		t.Fatalf("pool adds = %v, want one more for 192.168.100.7", fc.poolAdds)
	}
}
