package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/kube"
)

// fullMemory is a vDC with no room for another test worker.
func fullMemory() cloud.VDCCapacity {
	return cloud.VDCCapacity{MemoryMB: cloud.ResourceCapacity{Limit: 4096, Used: 4096}}
}

func TestScaleUpQueuesWhenCapacityIsShort(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs, want 0: %#v", len(fc.created), fc.created)
	}
	got := getPool(t, r, pool)
	if got.Status.ObservedReplicas != 2 || len(got.Status.Nodes) != 2 {
		t.Fatalf("observed=%d nodes=%d, want 2 and 2", got.Status.ObservedReplicas, len(got.Status.Nodes))
	}
	for _, n := range got.Status.Nodes {
		if n.Phase != infrav1.NodePhaseQueued {
			t.Fatalf("%s phase = %s, want Queued", n.Name, n.Phase)
		}
		if n.ProviderID != "" {
			t.Fatalf("%s has providerID %q, want none", n.Name, n.ProviderID)
		}
		if !strings.Contains(n.Message, "memory") || !strings.Contains(n.Message, "4096 MB") {
			t.Fatalf("%s message = %q, want the memory shortfall", n.Name, n.Message)
		}
	}
	if len(got.Status.Ledger.Used) != 2 {
		t.Fatalf("ledger used = %v, want both addresses reserved", got.Status.Ledger.Used)
	}
	cond := metaReady(got)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "NoResourceAvailable" {
		t.Fatalf("unexpected condition: %#v", cond)
	}
	if !strings.Contains(cond.Message, "queued for vDC capacity") {
		t.Fatalf("condition message = %q", cond.Message)
	}

	// A second pass must keep the slots queued: a slot without a VM is not a
	// worker that disappeared from vCD.
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	got = getPool(t, r, pool)
	if len(fc.created) != 0 {
		t.Fatalf("second pass created %d VMs, want 0", len(fc.created))
	}
	for _, n := range got.Status.Nodes {
		if n.Phase != infrav1.NodePhaseQueued {
			t.Fatalf("second pass: %s phase = %s, want Queued", n.Name, n.Phase)
		}
	}
}

func TestQueuedWorkerStartsWhenCapacityAppears(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs while the vDC was full", len(fc.created))
	}

	fc.capacity.MemoryMB.Used = 0
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile after freeing capacity: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1", len(fc.created))
	}
	got := getPool(t, r, pool)
	n := got.Status.Nodes[0]
	if n.Phase != infrav1.NodePhaseProvisioning || n.ProviderID == "" {
		t.Fatalf("queued worker did not start: %#v", n)
	}
	if n.Message != "" {
		t.Fatalf("message = %q, want empty after the create", n.Message)
	}
	if n.ProvisioningStartedAt == nil {
		t.Fatal("the provisioning clock was not started")
	}
}

func TestCapacityReadFailureDoesNotBlockCreates(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacityErr: errors.New("vCD unavailable")}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1: a failed read must not stop the pool", len(fc.created))
	}
	if got := getPool(t, r, pool); got.Status.Nodes[0].Phase != infrav1.NodePhaseProvisioning {
		t.Fatalf("phase = %s, want Provisioning", got.Status.Nodes[0].Phase)
	}
}

func TestCapacityPreflightCanBeTurnedOff(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	off := false
	pool.Spec.Capacity.Preflight = &off
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs, want 1 with capacity.preflight=false", len(fc.created))
	}
	if fc.capacityCalls != 0 {
		t.Fatalf("the vDC capacity was read %d times with preflight off", fc.capacityCalls)
	}
}

func TestScaleDownDropsQueuedSlots(t *testing.T) {
	ctx := context.Background()
	pool := testPool(0)
	pool.Spec.ScaleDown.MinReplicas = 0
	pool.Spec.ScaleDown.AllowLastWorker = true
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name: "pool-wrk-01", IP: "192.168.100.7", Phase: infrav1.NodePhaseQueued,
		Message: "waiting for vDC capacity: memory: need 4096 MB, 0 MB free",
	}}
	pool.Status.ObservedReplicas = 1
	pool.Status.Ledger = infrav1.IPLedger{Used: []string{"192.168.100.7"}}
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 0 || got.Status.ObservedReplicas != 0 {
		t.Fatalf("queued slot was not dropped: %#v", got.Status.Nodes)
	}
	if len(got.Status.Ledger.Used) != 0 {
		t.Fatalf("ledger used = %v, want the address released", got.Status.Ledger.Used)
	}
}

func rolloutPool(t *testing.T) (*infrav1.K8sWorkerPool, *fakeCloud, *fakeKube) {
	t.Helper()
	pool := testPool(1)
	pool.Spec.Flavor.CPUs = 4
	pool.Spec.Flavor.MemoryMB = 8192
	pool.Spec.Flavor.OSDiskGB = 20
	fc := &fakeCloud{vms: []cloud.VM{{
		ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7",
		CPU: 2, MemoryMB: 4096, OSDiskGB: 20, DiskGB: 30,
	}}}
	return pool, fc, &fakeKube{nodes: readyNodes("pool-wrk-01")}
}

func TestRolloutQueuesWhenCapacityIsShort(t *testing.T) {
	ctx := context.Background()
	pool, fc, fk := rolloutPool(t)
	fc.capacity = cloud.VDCCapacity{MemoryMB: cloud.ResourceCapacity{Limit: 8192, Used: 8192}}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fk.cordoned) != 0 {
		t.Fatalf("cordoned %v, want nothing taken out of service", fk.cordoned)
	}
	got := getPool(t, r, pool)
	if got.Status.Rollout.Phase != infrav1.RolloutPhaseQueued {
		t.Fatalf("rollout phase = %q, want Queued", got.Status.Rollout.Phase)
	}
	if got.Status.Nodes[0].Resize != nil {
		t.Fatalf("the worker was taken into the rollout: %#v", got.Status.Nodes[0].Resize)
	}
	if !strings.Contains(got.Status.Rollout.Message, "memory") {
		t.Fatalf("rollout message = %q, want the memory shortfall", got.Status.Rollout.Message)
	}
	cond := metaReady(got)
	if cond == nil || cond.Reason != "NoResourceAvailable" {
		t.Fatalf("unexpected condition: %#v", cond)
	}
}

func TestRolloutChecksTheDeltaNotTheWholeFlavor(t *testing.T) {
	ctx := context.Background()
	pool, fc, fk := rolloutPool(t)
	// Free memory covers the 4096 MB the resize adds, but not another 8192.
	fc.capacity = cloud.VDCCapacity{MemoryMB: cloud.ResourceCapacity{Limit: 12288, Used: 8192}}
	r := newTestReconciler(t, fc, fk, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if got.Status.Rollout.Phase == infrav1.RolloutPhaseQueued {
		t.Fatalf("the roll was queued although the delta fits: %q", got.Status.Rollout.Message)
	}
	if got.Status.Nodes[0].Resize == nil {
		t.Fatal("the worker was not taken into the rollout")
	}
	if len(fk.cordoned) != 1 {
		t.Fatalf("cordoned %v, want the worker cordoned", fk.cordoned)
	}
}

func TestResizeCapacityRequest(t *testing.T) {
	have := &infrav1.FlavorStatus{CPUs: 2, MemoryMB: 4096, OSDiskGB: 20}
	want := &infrav1.FlavorStatus{CPUs: 4, MemoryMB: 8192, OSDiskGB: 25}
	req := resizeCapacityRequest(have, want)
	if req.CPUs != 2 || req.MemoryMB != 4096 || req.StorageMB[""] != 5*1024 {
		t.Fatalf("unexpected delta: %#v", req)
	}
	// A shrink is not a resize: it asks for nothing.
	if req := resizeCapacityRequest(want, have); req.CPUs != 0 || req.MemoryMB != 0 || len(req.StorageMB) != 0 {
		t.Fatalf("unexpected delta for a shrink: %#v", req)
	}
	_ = kube.NodeInfo{}
}

// I-22: a dequeued slot must not keep reporting the shortfall that parked it
// while the VM is being created, which takes minutes.
func TestQueuedWorkerPublishesProgressBeforeTheVMCreate(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fc.capacity.MemoryMB.Used = 0
	var during *infrav1.K8sWorkerPool
	fc.createHook = func() {
		var p infrav1.K8sWorkerPool
		if err := r.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &p); err == nil {
			during = p.DeepCopy()
		}
	}
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile after freeing capacity: %v", err)
	}

	if during == nil {
		t.Fatal("CreateVM never ran")
	}
	st := during.Status.Nodes[0]
	if st.Phase != infrav1.NodePhaseQueued {
		t.Fatalf("phase during the create = %s, want Queued until the VM exists", st.Phase)
	}
	if !strings.Contains(st.Message, "creating the VM") {
		t.Fatalf("message during the create = %q, want the progress message", st.Message)
	}

	n := getPool(t, r, pool).Status.Nodes[0]
	if n.Phase != infrav1.NodePhaseProvisioning || n.ProviderID == "" {
		t.Fatalf("slot after the create = %#v, want Provisioning with a VM id", n)
	}
}

// A create that fails after a dequeue leaves the slot queued, with the failure
// reported, so the next pass retries it instead of parking it Failed.
func TestQueuedWorkerStaysQueuedWhenTheCreateFails(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacity: fullMemory()}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fc.capacity.MemoryMB.Used = 0
	fc.failAt = 1
	if _, err := r.Reconcile(ctx, requestFor(pool)); err == nil {
		t.Fatal("the reconcile reported success after a failed create")
	}

	n := getPool(t, r, pool).Status.Nodes[0]
	if n.Phase != infrav1.NodePhaseQueued {
		t.Fatalf("phase after a failed create = %s, want Queued", n.Phase)
	}
	if !strings.Contains(n.Message, "the create failed") {
		t.Fatalf("message after a failed create = %q", n.Message)
	}
}
