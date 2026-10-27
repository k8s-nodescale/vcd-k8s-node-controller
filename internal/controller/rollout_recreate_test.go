package controller

import (
	"strings"
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// pass runs n reconcile passes, which is how a roll advances one step per pass.
func pass(t *testing.T, r *WorkerPoolReconciler, pool *infrav1.K8sWorkerPool, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		reconcileOnce(t, r, pool)
	}
}

// Roadmap 8: vCD cannot shrink a VM, so a smaller spec.flavor replaces the
// worker - same name, same address, a new VM - and ResizeVM is never asked for
// something it refuses.
func TestShrinkRecreatesTheWorkerInTheSameSlot(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Flavor.CPUs = 4
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	oldID := fc.vms[0].ID
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 2 })

	// The worker is drained first, then the VM is scheduled for deletion.
	reconcileOnce(t, r, pool)
	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || !n.Resize.Recreate || n.Resize.Phase != resizePhaseDeleting {
		t.Fatalf("pool-wrk-01 = %#v, want a recreate in phase %s", n, resizePhaseDeleting)
	}
	if len(fk.drained) != 1 {
		t.Fatalf("drained %v, want the worker drained before it is replaced", fk.drained)
	}

	// The VM goes and the slot stays: same address, no VM, not Missing.
	reconcileOnce(t, r, pool)
	n = nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseCreating {
		t.Fatalf("pool-wrk-01 = %#v, want the slot kept in phase %s", n, resizePhaseCreating)
	}
	if n.Phase == infrav1.NodePhaseMissing {
		t.Fatalf("the slot of a recreating worker was marked Missing: %#v", n)
	}
	if n.IP != "192.168.100.7" || n.ProviderID != "" {
		t.Fatalf("slot = %#v, want the same address and no VM", n)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != oldID {
		t.Fatalf("deleted %v, want the old VM %s", fc.deleted, oldID)
	}
	if len(fk.deletedNodes) != 1 {
		t.Fatalf("deleted nodes %v, want the stale Node object gone before the VM", fk.deletedNodes)
	}

	// The replacement is created in the same slot, with the new shape.
	reconcileOnce(t, r, pool)
	n = nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseBooting {
		t.Fatalf("pool-wrk-01 = %#v, want phase %s", n, resizePhaseBooting)
	}
	if n.ProviderID == "" {
		t.Fatal("the replacement VM is not recorded in the slot")
	}
	if n.IP != "192.168.100.7" {
		t.Fatalf("ip = %q, want the same address", n.IP)
	}
	if len(fc.created) != 2 {
		t.Fatalf("created %d VMs, want 2", len(fc.created))
	}
	if got := fc.created[1]; got.Name != "pool-wrk-01" || got.IP != "192.168.100.7" || got.CPUs != 2 {
		t.Fatalf("replacement = %+v, want pool-wrk-01 at 192.168.100.7 with 2 vCPU", got)
	}
	if len(fc.resizes) != 0 {
		t.Fatalf("resized %v, want a recreate instead", fc.resizes)
	}

	// And it comes back into service.
	reconcileOnce(t, r, pool)
	n = nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize != nil || n.Phase != infrav1.NodePhaseReady {
		t.Fatalf("pool-wrk-01 = %#v, want it Ready with no resize left", n)
	}
}

// Roadmap 8: another spec.flavor.template is another image, which no in-place
// change can reach either.
func TestTemplateChangeRecreatesTheWorker(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	if fc.created[0].Template != "ubuntu" {
		t.Fatalf("the first VM was cloned from %q, want ubuntu", fc.created[0].Template)
	}
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.Template = "ubuntu-24.04" })

	reconcileOnce(t, r, pool)
	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || !n.Resize.Recreate || n.Resize.Phase != resizePhaseDeleting {
		t.Fatalf("pool-wrk-01 = %#v, want a recreate in phase %s", n, resizePhaseDeleting)
	}

	pass(t, r, pool, 3) // delete, create, come back
	n = nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize != nil || n.Phase != infrav1.NodePhaseReady {
		t.Fatalf("pool-wrk-01 = %#v, want it Ready after the recreate", n)
	}
	if len(fc.created) != 2 || fc.created[1].Template != "ubuntu-24.04" {
		t.Fatalf("created %+v, want the second VM cloned from ubuntu-24.04", fc.created)
	}
	if len(fc.resizes) != 0 {
		t.Fatalf("resized %v, want a recreate instead", fc.resizes)
	}
}

// An adopted worker carries no record of its image, and an unknown image is no
// opinion: adoption must never turn into a replacement.
func TestAdoptedWorkerWithAnUnknownTemplateIsNotRecreated(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Flavor.CPUs = 4
	fc := &fakeCloud{vms: []cloud.VM{{
		ID: "vm-pool-wrk-01", Name: "pool-wrk-01", IP: "192.168.100.7",
		CPU: 4, MemoryMB: 4096, PoweredOn: true, // no Template: created outside
	}}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	pass(t, r, pool, 2)

	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize != nil || n.Phase != infrav1.NodePhaseReady {
		t.Fatalf("pool-wrk-01 = %#v, want an untouched adopted worker", n)
	}
	if len(fc.deleted) != 0 || len(fc.created) != 0 {
		t.Fatalf("deleted %v and created %d VMs, want neither", fc.deleted, len(fc.created))
	}
}

// A recreate frees the capacity it needs by deleting first, so a full vDC must
// not park it in Queued the way an in-place grow is parked.
func TestRecreateDoesNotWaitForFreeCapacity(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	// The vDC is full: a grow could not fit, and this change is a grow plus a
	// new image, so only the recreate path keeps it moving.
	fc.capacity = cloud.PlatformCapacity{
		CPUMHzPerCore: 2300,
		CPU:           cloud.ResourceCapacity{Limit: 4600, Used: 4600},
		MemoryMB:      cloud.ResourceCapacity{Limit: 8192, Used: 8192},
	}
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.Flavor.CPUs = 4
		p.Spec.Flavor.Template = "ubuntu-24.04"
	})

	reconcileOnce(t, r, pool)
	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-01")
	if n == nil || n.Resize == nil || !n.Resize.Recreate {
		t.Fatalf("pool-wrk-01 = %#v, want a recreate to start despite the full vDC", n)
	}
	if got.Status.Rollout.Phase == infrav1.RolloutPhaseQueued {
		t.Fatalf("rollout = %#v, want it to run", got.Status.Rollout)
	}
}

// A create that fails after the old VM is already gone must not park the slot
// with no VM: it keeps the phase, reports the reason and retries, the way a
// failed scale-up does.
func TestRecreateRetriesAFailedCreate(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Flavor.CPUs = 4
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Flavor.CPUs = 2 })
	pass(t, r, pool, 2) // drain, then delete

	fc.failAt = 2 // the replacement is the second create of the test
	reconcileOnce(t, r, pool)

	n := nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize == nil || n.Resize.Phase != resizePhaseCreating {
		t.Fatalf("pool-wrk-01 = %#v, want the slot kept in phase %s", n, resizePhaseCreating)
	}
	if n.Phase == infrav1.NodePhaseFailed || n.Resize.Phase == resizePhaseFailed {
		t.Fatalf("the slot was parked Failed with no VM: %#v", n)
	}
	if !strings.Contains(n.Resize.Message, "injected create failure") {
		t.Fatalf("message = %q, want the create failure named", n.Resize.Message)
	}

	pass(t, r, pool, 2) // create, then come back into service
	n = nodeByName(getPool(t, r, pool), "pool-wrk-01")
	if n == nil || n.Resize != nil || n.Phase != infrav1.NodePhaseReady {
		t.Fatalf("pool-wrk-01 = %#v, want it Ready after the retry", n)
	}
}
